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

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
)

// Tests for the interaction lifecycle shared by approvals, trust dialogs and
// questions: every pending interaction is keyed by its exact prompt signature,
// retired when the pane stops showing it, bounded by a timeout, and a spawn
// marker has exactly one owner.

const lifecycleRule = "────────────────────────────────────────────────────────────────────────"

func questionPaneText(prompt string, options ...string) string {
	var b strings.Builder
	b.WriteString(lifecycleRule + "\n☐ Pick\n\n" + prompt + "\n\n")
	for i, o := range options {
		prefix := "  "
		if i == 0 {
			prefix = "❯ "
		}
		fmt.Fprintf(&b, "%s%d. %s\n", prefix, i+1, o)
	}
	fmt.Fprintf(&b, "  %d. Type something.\n%s\n  Chat about this\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n", len(options)+1, lifecycleRule)
	return b.String()
}

const idlePaneText = lifecycleRule + "\n❯ \n" + lifecycleRule + "\n  ? for shortcuts\n"

func countSent(dc *recordingClient, substr string) int {
	n := 0
	dc.mu.Lock()
	defer dc.mu.Unlock()
	for _, s := range dc.sent {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func edited(dc *recordingClient, substr string) bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	for _, s := range dc.edits {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// TestQuestionAnsweredInTerminalRetiresPendingEntry: answering or cancelling the
// picker in the terminal retires its pending entry, so the next question posts.
func TestQuestionAnsweredInTerminalRetiresPendingEntry(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	var tr promptTracker
	q1 := questionPaneText("Which color?", "Red", "Green")

	w.pollPrompts("thread-1", "t:0.0", q1, &tr)
	if countSent(dc, "Which color?") != 1 || !w.pendingInteractions.has("thread-1", "t:0.0", "question") {
		t.Fatalf("first question not posted: %q", dc.final())
	}
	// Still pending while the pane still shows it: no duplicate post.
	w.pollPrompts("thread-1", "t:0.0", q1, &tr)
	if countSent(dc, "Which color?") != 1 {
		t.Fatalf("duplicate post: %q", dc.final())
	}

	// Answered / cancelled in the terminal: the picker is gone.
	for i := 0; i < promptAbsentPolls; i++ {
		w.pollPrompts("thread-1", "t:0.0", idlePaneText, &tr)
	}
	if w.pendingInteractions.has("thread-1", "t:0.0", "question") {
		t.Fatal("pending question survived the picker disappearing")
	}
	if !edited(dc, "Question expired") {
		t.Fatalf("stale question message not marked expired: %q", dc.final())
	}

	// The next question (even an identical one) posts.
	w.pollPrompts("thread-1", "t:0.0", questionPaneText("Which size?", "S", "L"), &tr)
	if countSent(dc, "Which size?") != 1 {
		t.Fatalf("next question did not post: %q", dc.final())
	}
	for i := 0; i < promptAbsentPolls; i++ {
		w.pollPrompts("thread-1", "t:0.0", idlePaneText, &tr)
	}
	w.pollPrompts("thread-1", "t:0.0", questionPaneText("Which size?", "S", "L"), &tr)
	if countSent(dc, "Which size?") != 2 {
		t.Fatalf("identical question after a retirement did not post: %q", dc.final())
	}

	// A different picker replacing the pending one retires it at once.
	w.pollPrompts("thread-1", "t:0.0", questionPaneText("Which shape?", "Round", "Square"), &tr)
	if countSent(dc, "Which shape?") != 1 {
		t.Fatalf("replacement question did not post: %q", dc.final())
	}
	for _, e := range w.pendingInteractions.entries {
		if e.kind == "question" && !strings.Contains(e.signature, "Which shape?") {
			t.Fatalf("stale entry for %q still pending", e.signature)
		}
	}
}

// TestApprovalRetiredWhenPromptChangesOrVanishes: same invariant for approvals.
func TestApprovalRetiredWhenPromptChangesOrVanishes(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	var tr promptTracker
	w.pollPrompts("thread-1", "t:0.0", permPane("ls /tmp"), &tr)
	if !w.pendingInteractions.has("thread-1", "t:0.0", "approval") {
		t.Fatal("approval not posted")
	}
	w.pollPrompts("thread-1", "t:0.0", permPane("rm -rf /tmp/x"), &tr)
	if countSent(dc, "rm -rf /tmp/x") != 1 {
		t.Fatalf("changed prompt did not post: %q", dc.final())
	}
	for _, e := range w.pendingInteractions.entries {
		if e.kind == "approval" && e.signature != "rm -rf /tmp/x" {
			t.Fatalf("stale approval entry %q still pending", e.signature)
		}
	}
	for i := 0; i < promptAbsentPolls; i++ {
		w.pollPrompts("thread-1", "t:0.0", idlePaneText, &tr)
	}
	if w.pendingInteractions.has("thread-1", "t:0.0", "approval") {
		t.Fatal("pending approval survived the prompt disappearing")
	}
}

// TestPendingQuestionTimesOut: an unanswered question is retired by its timeout
// and does not block the next one.
func TestPendingQuestionTimesOut(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.ApprovalTimeout = 100 * time.Millisecond
	pane := &fakePane{}
	pane.set(questionPaneText("Which color?", "Red", "Green"))
	w.capturePane = pane.capture
	var tr promptTracker

	w.pollPrompts("thread-1", "t:0.0", pane.text, &tr)
	if !w.pendingInteractions.has("thread-1", "t:0.0", "question") {
		t.Fatal("question not posted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && w.pendingInteractions.has("thread-1", "t:0.0", "question") {
		time.Sleep(20 * time.Millisecond)
	}
	if w.pendingInteractions.has("thread-1", "t:0.0", "question") {
		t.Fatal("unanswered question never timed out")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !edited(dc, "Question timed out") {
		time.Sleep(20 * time.Millisecond)
	}
	if !edited(dc, "Question timed out") {
		t.Fatalf("timed-out question message not updated: %q", dc.final())
	}
	// The next question posts.
	w.cfg.ApprovalTimeout = time.Minute
	w.pollPrompts("thread-1", "t:0.0", questionPaneText("Which size?", "S", "L"), &tr)
	if countSent(dc, "Which size?") != 1 {
		t.Fatalf("next question blocked after a timeout: %q", dc.final())
	}
}

// TestTrustRequestRetiredWhenDialogVanishes: answering the trust dialog in the
// terminal retires the Discord request instead of leaving live buttons behind.
func TestTrustRequestRetiredWhenDialogVanishes(t *testing.T) {
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.ApprovalTimeout = time.Minute
	pane := &fakePane{}
	pane.set("Quick safety check: Is this a project you created or one you trust?\n\n❯ No, exit\n  Yes, I trust this folder\n")
	w.capturePane = pane.capture
	done := make(chan bool, 1)
	go func() { done <- w.askTrust(context.Background(), "t:0.0", "chan") }()
	time.Sleep(700 * time.Millisecond)
	pane.set(idlePaneText)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("askTrust did not retire after the dialog vanished")
	}
	if w.pendingInteractions.has("chan", "t:0.0", "trust") {
		t.Fatal("trust entry still pending")
	}
	if !edited(dc, "Trust request expired") {
		t.Fatalf("trust message not marked expired: %q", dc.final())
	}
}

// TestScanSessionsNeverClaimsOwnedMarker: a marker owned by a spawn path is not
// claimed by the scanner even with a transcript and a live pane.
func TestScanSessionsNeverClaimsOwnedMarker(t *testing.T) {
	target := tmuxPane(t, "llm-test-owned", `sleep 30`)
	root := t.TempDir()
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, root)
	data, _ := json.Marshal(spawnMarkerJSON{Target: target, SessionID: testSessionID, Owner: markerOwnerSpawn})
	p := filepath.Join(w.cfg.StateDir, "pending-owned")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "-p")
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte(`{"type":"user","uuid":"u","message":{"role":"user","content":"x"}}`+"\n"), 0600)
	seen := map[string]bool{}
	w.scanSessions(context.Background(), seen)
	time.Sleep(100 * time.Millisecond)
	if seen[testSessionID] || len(dc.final()) != 0 {
		t.Fatalf("scanner claimed an owned marker: %q", dc.final())
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("owned marker must be left to its owner")
	}
}

// TestSpawnBindTimeoutFailsClosedWithScannerRunning: a transcript that appears
// after the spawn's bind deadline (while the scanner is polling) must not
// produce a thread, and the spawned pane is torn down.
func TestSpawnBindTimeoutFailsClosedWithScannerRunning(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	root := t.TempDir()
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, root)
	w.injector = inject.New(w.cfg.StateDir)
	// 1100ms is not a multiple of the 200ms transcript poll, so the handler
	// notices the deadline ~100ms after it passes: the window the scanner used to win.
	w.cfg.SpawnBindTimeout = 1100 * time.Millisecond
	gotFile := filepath.Join(t.TempDir(), "got")
	name := fmt.Sprintf("llm-tspawn-%d", os.Getpid())
	target := name + ":0.0"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })

	// Pane: ready frame, then block for the first prompt and record its arrival.
	// The transcript is never written by the pane; the test writes it late.
	pane := fmt.Sprintf(`printf '%%s\n' ' Claude Code' '' '%s' '❯ ' '%s' '  ? for shortcuts'; read line; touch %s; sleep 30`, testRule, testRule, gotFile)
	llmux := filepath.Join(t.TempDir(), "llmux")
	script := fmt.Sprintf(`#!/bin/sh
printf '{"target":"%s","spawnInstant":1,"sessionID":"%s","owner":"%%s"}' "$RELAY_MARKER_OWNER" > "$RELAY_STATE_DIR/pending-%s"
tmux new-session -d -s %s -x 100 -y 30 sh -c %q
echo %s
`, target, testSessionID, name, name, pane, name)
	if err := os.WriteFile(llmux, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	w.cfg.LlmuxBin = llmux

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var scanWG sync.WaitGroup
	scanWG.Add(1)
	go func() { // the scanner, as Run drives it
		defer scanWG.Done()
		seen := map[string]bool{}
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				w.claimed.copyInto(seen)
				w.scanSessions(ctx, seen)
			}
		}
	}()
	// The transcript appears just after the bind deadline, before the teardown.
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(gotFile); err == nil {
				time.Sleep(w.cfg.SpawnBindTimeout + 20*time.Millisecond)
				dir := filepath.Join(root, "-p")
				_ = os.MkdirAll(dir, 0755)
				_ = os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte(`{"type":"user","uuid":"u","message":{"role":"user","content":"late"}}`+"\n"), 0600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	err := w.handleSpawnFromDiscord(ctx, "hello", nil)
	if err == nil || !strings.Contains(err.Error(), "never produced a transcript") {
		t.Fatalf("handleSpawnFromDiscord = %v, want bind-timeout failure", err)
	}
	time.Sleep(500 * time.Millisecond) // give a (buggy) scanner time to create a thread
	cancel()
	scanWG.Wait()
	for _, m := range dc.final() {
		if strings.Contains(m, "**Session**") {
			t.Fatalf("a thread was created for a timed-out spawn: %q", dc.final())
		}
	}
	if rec := w.sessions.Get(testSessionID); rec != nil {
		t.Fatalf("timed-out spawn was recorded: %+v", rec)
	}
	if isTmuxPaneAlive("=" + target) {
		t.Fatal("spawned pane was not torn down")
	}
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, "pending-"+name)); !os.IsNotExist(err) {
		t.Fatal("marker of the failed spawn was not removed")
	}
}
