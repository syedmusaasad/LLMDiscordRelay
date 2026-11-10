package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

const testSessionID = "11111111-2222-4333-8444-555555555555"

// recordingClient is a DiscordClient that records every write.
type recordingClient struct {
	mu         sync.Mutex
	nextID     int
	sent       []string          // content of every SendMessage / SendComponents
	edits      map[string]string // message id -> latest content
	sentIDs    []string
	components [][]Component
	onInter    InteractionHandler
}

func newRecordingClient() *recordingClient { return &recordingClient{edits: map[string]string{}} }

func (c *recordingClient) SendMessage(channelID, content string) (MsgRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := fmt.Sprintf("m%d", c.nextID)
	c.sent = append(c.sent, content)
	c.sentIDs = append(c.sentIDs, id)
	c.edits[id] = content
	return MsgRef{ID: id, ChannelID: channelID}, nil
}
func (c *recordingClient) EditMessage(channelID, messageID, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits[messageID] = content
	return nil
}
func (c *recordingClient) CreateThread(channelID, messageID, name string) (ThreadRef, error) {
	return ThreadRef{ID: "thread-1"}, nil
}
func (c *recordingClient) SendTyping(string) error            { return nil }
func (c *recordingClient) RenameThread(_, _ string) error     { return nil }
func (c *recordingClient) OnMessage(MessageHandler)           {}
func (c *recordingClient) OnInteraction(h InteractionHandler) { c.onInter = h }
func (c *recordingClient) AcknowledgeInteraction(InteractionEvent) error {
	return nil
}
func (c *recordingClient) SendComponents(channelID, content string, comps []Component) (MsgRef, error) {
	ref, err := c.SendMessage(channelID, content)
	c.mu.Lock()
	c.components = append(c.components, comps)
	c.mu.Unlock()
	return ref, err
}
func (c *recordingClient) EditComponents(channelID, messageID, content string, _ []Component) error {
	return c.EditMessage(channelID, messageID, content)
}

// final returns the latest content of each sent message, in send order.
func (c *recordingClient) final() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sentIDs))
	for _, id := range c.sentIDs {
		out = append(out, c.edits[id])
	}
	return out
}

func newTestWatcher(t *testing.T, dc DiscordClient, root string) *Watcher {
	t.Helper()
	stateDir := t.TempDir()
	sessions, err := LoadSessions(filepath.Join(stateDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Watcher{
		cfg:      Config{StateDir: stateDir, ChannelID: "chan", ProjectsRoot: root, ApprovalTimeout: 2 * time.Second},
		store:    transcript.Store{Root: root},
		discord:  dc,
		sessions: sessions,
		throttle: NewThrottle(1000, time.Second),
		runCtx:   context.Background(),
	}
}

func replay(w *Watcher, evs []transcript.Event) {
	asst := map[string]*msgState{}
	tools := map[string]*toolState{}
	seen := map[string]bool{}
	var turn turnState
	for _, ev := range evs {
		w.mirrorEvent(testSessionID, "thread-1", ev, asst, tools, &turn, seen)
	}
}

func TestMirrorEventFixture(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "claude")
	p, err := transcript.Store{Root: root}.Path(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := transcript.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	dc := newRecordingClient()
	replay(newTestWatcher(t, dc, root), evs)
	got := dc.final()
	want := []string{
		"**You:** Create hello.txt containing hi, then run: ls -la",
		"🔧 **Tool:** `Bash` | ✅ done | ID: `toolu_01AnBXLjfaCfcncKYUbr5xof`",
		"I created `hello.txt`",
		"_— 1 tool(s)_",
		"**You:** Run this exact bash command: rm hello.txt",
		"🔧 **Tool:** `Bash` | ❌ failed | ID: `toolu_01SciyKT2hr4hNtfbGPeQrrB`", // interrupted: not softened
		"_Turn interrupted_",
		"_— 1 tool(s)_",
		"**You:** Use the AskUserQuestion tool",
	}
	if len(got) != len(want) {
		t.Fatalf("messages (%d) = %q\nwant %d", len(got), got, len(want))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("message %d = %q, want it to contain %q", i, got[i], w)
		}
	}
}

func TestMirrorEventStreamsOneMessagePerAssistantID(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	ev := func(k transcript.Kind, uuid, msg, text string) transcript.Event {
		return transcript.Event{Kind: k, UUID: uuid, MessageID: msg, Text: text}
	}
	replay(w, []transcript.Event{
		ev(transcript.KindUserPrompt, "u0", "u0", "hi"),
		ev(transcript.KindAssistantText, "a1", "msg_A", "first"),
		ev(transcript.KindAssistantText, "a2", "msg_A", "second"),
		ev(transcript.KindAssistantText, "a2", "msg_A", "second"), // duplicate line: ignored
		ev(transcript.KindAssistantText, "b1", "msg_B", "other message"),
		{Kind: transcript.KindTurnEnd, UUID: "t"},
	})
	got := dc.final()
	want := []string{"**You:** hi", "first\n\nsecond", "other message", "_— 0 tool(s)_"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

func TestMirrorEventTurnFinishedWithoutOutput(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	replay(w, []transcript.Event{
		{Kind: transcript.KindUserPrompt, UUID: "u", Text: "hi"},
		{Kind: transcript.KindTurnEnd, UUID: "t"},
		{Kind: transcript.KindTurnEnd, UUID: "t2"}, // no open turn: ignored
	})
	want := "**You:** hi|_Turn finished without output_|_— 0 tool(s)_"
	if got := strings.Join(dc.final(), "|"); got != want {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

func TestMirrorEventSoftensFailedToolOnCompletedTurn(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	replay(w, []transcript.Event{
		{Kind: transcript.KindUserPrompt, UUID: "u", Text: "go"},
		{Kind: transcript.KindToolCall, UUID: "a", MessageID: "m", ToolID: "toolu_1", ToolName: "Read"},
		{Kind: transcript.KindToolResult, UUID: "r", ToolID: "toolu_1", IsError: true},
		{Kind: transcript.KindAssistantText, UUID: "a2", MessageID: "m2", Text: "recovered"},
		{Kind: transcript.KindTurnEnd, UUID: "t"},
	})
	got := dc.final()
	if len(got) != 4 || !strings.Contains(got[1], "⚠️ warning") {
		t.Fatalf("messages = %q", got)
	}
}

func TestScanSessionsPairsMarkerWithTranscript(t *testing.T) {
	root := t.TempDir()
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, root)
	writeMarker := func(name string, m spawnMarkerJSON) string {
		data, _ := json.Marshal(m)
		p := filepath.Join(w.cfg.StateDir, "pending-"+name)
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	other := "99999999-2222-4333-8444-555555555555"
	// No transcript yet: nothing is claimed.
	writeMarker("early", spawnMarkerJSON{Target: "nosuch-early:0.0", SessionID: testSessionID, Resume: true})
	// Malformed id: ignored outright.
	writeMarker("bad", spawnMarkerJSON{Target: "nosuch-bad:0.0", SessionID: "../../etc/passwd", Resume: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := map[string]bool{}
	w.scanSessions(ctx, seen)
	time.Sleep(100 * time.Millisecond)
	if len(dc.final()) != 0 || seen[testSessionID] {
		t.Fatalf("claimed a session with no transcript: %q", dc.final())
	}

	// The first prompt creates the transcript: the marker now binds.
	dir := filepath.Join(root, "-work-demo")
	_ = os.MkdirAll(dir, 0755)
	line := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	// A transcript of another session must not bind this marker.
	if err := os.WriteFile(filepath.Join(dir, other+".jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	w.scanSessions(ctx, seen)
	if !seen[testSessionID] || seen[other] {
		t.Fatalf("seen = %v", seen)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := strings.Join(dc.final(), "|")
		if strings.Contains(got, "**You:** hello") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := dc.final()
	if len(got) < 2 || !strings.Contains(got[0], testSessionID) || !strings.Contains(strings.Join(got, "|"), "**You:** hello") {
		t.Fatalf("mirrored messages = %q", got)
	}
	rec := w.sessions.Get(testSessionID)
	if rec == nil || rec.ThreadID != "thread-1" || rec.TmuxTarget != "nosuch-early:0.0" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestNewSpawnMarkerNeedsLivePane(t *testing.T) {
	root := t.TempDir()
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, root)
	data, _ := json.Marshal(spawnMarkerJSON{Target: "nosuch-dead:0.0", SessionID: testSessionID})
	if err := os.WriteFile(filepath.Join(w.cfg.StateDir, "pending-dead"), data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "-p")
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte(`{"type":"user","uuid":"u","message":{"role":"user","content":"x"}}`+"\n"), 0600)
	seen := map[string]bool{}
	w.scanSessions(context.Background(), seen)
	if seen[testSessionID] || len(dc.final()) != 0 {
		t.Fatal("a new-spawn marker with a dead pane must not be claimed")
	}
}

func TestStaleDeadMarkerIsRemoved(t *testing.T) {
	w := newTestWatcher(t, newRecordingClient(), t.TempDir())
	data, _ := json.Marshal(spawnMarkerJSON{Target: "nosuch-stale:0.0", SessionID: testSessionID})
	p := filepath.Join(w.cfg.StateDir, "pending-stale")
	_ = os.WriteFile(p, data, 0600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	w.scanSessions(context.Background(), map[string]bool{})
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("orphaned marker should be removed")
	}
}

func TestFormatAge(t *testing.T) {
	for ms, want := range map[int64]string{5_000: "5s ago", 120_000: "2m ago", 3 * 3600_000: "3h ago", 50 * 3600_000: "2d ago"} {
		if got := formatAge(ms); got != want {
			t.Errorf("formatAge(%d) = %q, want %q", ms, got, want)
		}
	}
}

// ── tmux-backed readiness tests ─────────────────────────────────────────────

func tmuxPane(t *testing.T, name, script string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", name).Run() })
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "100", "-y", "30", "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v\n%s", err, out)
	}
	return name + ":0.0"
}

const testRule = "────────────────────────────────────────────────────────────────"

func TestAwaitReadyDetectsReadyPane(t *testing.T) {
	target := tmuxPane(t, "llm-test-ready", fmt.Sprintf(`printf '%%s\n' ' Claude Code' '' '%s' '❯ ' '%s' '  ? for shortcuts'; sleep 30`, testRule, testRule))
	w := newTestWatcher(t, newRecordingClient(), t.TempDir())
	if err := w.awaitReady(context.Background(), target, "chan"); err != nil {
		t.Fatalf("awaitReady = %v", err)
	}
}

func TestAwaitReadyDetectsResumeRefusal(t *testing.T) {
	target := tmuxPane(t, "llm-test-refused", `echo "No conversation found with session ID: x"; sleep 30`)
	w := newTestWatcher(t, newRecordingClient(), t.TempDir())
	err := w.awaitReady(context.Background(), target, "chan")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("awaitReady = %v, want refusal", err)
	}
}

func TestAwaitReadyPaneDiesBeforeReady(t *testing.T) {
	target := tmuxPane(t, "llm-test-dies", `exit 1`)
	w := newTestWatcher(t, newRecordingClient(), t.TempDir())
	err := w.awaitReady(context.Background(), target, "chan")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("awaitReady = %v, want refusal-by-death", err)
	}
}

func TestTrustDialogNeedsDiscordApproval(t *testing.T) {
	// The pane reads one key: "Down" then Enter are what TrustAcceptRecipe sends.
	script := `printf 'Quick safety check: Is this a project you created or one you trust?\n\n❯ No, exit\n  Yes, I trust this folder\n'; stty -icanon -echo; dd bs=1 count=4 2>/dev/null >/dev/null; printf '\033[2J\033[H'; printf '%s\n' ' Claude Code' '' '` + testRule + `' '❯ ' '` + testRule + `' '  ? for shortcuts'; sleep 30`
	target := tmuxPane(t, "llm-test-trust", script)
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	dc.OnInteraction(w.handleInteraction)
	w.cfg.AllowedUserID = "me"

	done := make(chan error, 1)
	go func() { done <- w.awaitReady(context.Background(), target, "chan") }()

	// Nothing may be accepted until the user approves.
	deadline := time.Now().Add(5 * time.Second)
	var approveID string
	for time.Now().Before(deadline) && approveID == "" {
		dc.mu.Lock()
		if len(dc.components) > 0 {
			for _, c := range dc.components[0] {
				if strings.HasPrefix(c.CustomID, "trust:approve:") {
					approveID = c.CustomID
				}
			}
		}
		dc.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	if approveID == "" {
		t.Fatal("trust dialog was not surfaced to Discord")
	}
	select {
	case err := <-done:
		t.Fatalf("awaitReady returned %v before approval", err)
	case <-time.After(500 * time.Millisecond):
	}
	dc.onInter(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: approveID})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("awaitReady after approval = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("awaitReady did not finish after approval")
	}
}

func TestTrustDialogDenyDismisses(t *testing.T) {
	target := tmuxPane(t, "llm-test-trustdeny", `printf 'Quick safety check: Is this a project you created or one you trust?\n\n❯ No, exit\n  Yes, I trust this folder\n'; sleep 30`)
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	dc.OnInteraction(w.handleInteraction)
	w.cfg.AllowedUserID = "me"
	done := make(chan error, 1)
	go func() { done <- w.awaitReady(context.Background(), target, "chan") }()
	var denyID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && denyID == "" {
		dc.mu.Lock()
		if len(dc.components) > 0 {
			for _, c := range dc.components[0] {
				if strings.HasPrefix(c.CustomID, "trust:deny:") {
					denyID = c.CustomID
				}
			}
		}
		dc.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	if denyID == "" {
		t.Fatal("trust dialog was not surfaced")
	}
	dc.onInter(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: denyID})
	select {
	case err := <-done:
		if err != errTrustDenied {
			t.Fatalf("awaitReady = %v, want errTrustDenied", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no result after deny")
	}
}

func TestTrustDialogTimeoutDenies(t *testing.T) {
	target := tmuxPane(t, "llm-test-trusttimeout", `printf 'Quick safety check: Is this a project you created or one you trust?\n\n❯ No, exit\n  Yes, I trust this folder\n'; sleep 30`)
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.ApprovalTimeout = 300 * time.Millisecond
	err := w.awaitReady(context.Background(), target, "chan")
	if err != errTrustDenied {
		t.Fatalf("awaitReady = %v, want errTrustDenied after timeout", err)
	}
}

func TestHandleAmbiguousResumeSplitsLongList(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-work-demo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("aaaaaaaa-bbbb-4ccc-8ddd-%012d", i)
		ids = append(ids, id)
		title := strings.Repeat("t", 80)
		body := fmt.Sprintf(`{"type":"ai-title","aiTitle":%q}`+"\n", title)
		if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, root)
	w.handleAmbiguousResume(&SessionRecord{ThreadID: "thread-1"}, "hi", nil)

	got := dc.final()
	if len(got) < 2 {
		t.Fatalf("expected the list to be split across messages, got %d", len(got))
	}
	all := strings.Join(got, "\n")
	for i, m := range got {
		if n := len([]rune(m)); n > 2000 {
			t.Errorf("message %d has %d chars, over Discord's 2000 limit", i, n)
		}
	}
	for _, id := range ids {
		if !strings.Contains(all, id) {
			t.Errorf("candidate %s missing from posted messages", id)
		}
	}
	for n := 1; n <= 20; n++ {
		if !strings.Contains(all, fmt.Sprintf("%d. `", n)) {
			t.Errorf("entry number %d missing", n)
		}
	}
}

func TestWaitForTranscriptDeadPaneWithExistingTranscriptFails(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "claude")
	w := newTestWatcher(t, newRecordingClient(), root)
	if _, err := w.store.Path(testSessionID); err != nil {
		t.Fatalf("fixture transcript must exist: %v", err)
	}
	err := w.waitForTranscript(context.Background(), testSessionID, "no-such-session-xyz:0.0", 5*time.Second)
	if err == nil {
		t.Fatal("waitForTranscript succeeded for a dead pane; it must fail closed")
	}
	if !strings.Contains(err.Error(), "died") {
		t.Errorf("err = %v, want pane-died error", err)
	}
}

// A prompt the relay had to inject twice is echoed to Discord exactly once: the
// first transcript event is posted (the first send may have been lost, leaving
// it as the only one) and only a second event is suppressed.
func TestSuppressedEchoStore_FirstEchoPostedDuplicateSuppressed(t *testing.T) {
	var s suppressedEchoStore
	if s.suppress("sess", "hello") {
		t.Fatal("an unarmed prompt must never be suppressed")
	}
	s.add("sess", "hello")
	if s.suppress("other", "hello") {
		t.Fatal("suppression leaked to another session")
	}
	if s.suppress("sess", "hello") {
		t.Fatal("the first echo after a retry must still be posted")
	}
	if !s.suppress("sess", "hello") {
		t.Fatal("the duplicate echo must be suppressed")
	}
	if s.suppress("sess", "hello") {
		t.Fatal("a later, genuinely repeated prompt must be posted")
	}
}
