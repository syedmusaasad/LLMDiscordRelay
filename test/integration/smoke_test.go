//go:build integration

// Package integration contains integration tests that require tmux,
// fake-claude, llmux and mock-discord to be on PATH.
//
// Run via: make integration
// Or directly: go test -tags integration ./test/integration/...
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

// ── shared fixtures ───────────────────────────────────────────────────────────

// mustBin returns the path to a binary that must be on PATH.
func mustBin(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("binary %q not found on PATH: %v", name, err)
	}
	return p
}

// testSID returns a well-formed Claude Code session id (a UUID) for fixture n.
func testSID(n int) string {
	return fmt.Sprintf("aaaaaaaa-1111-4111-8111-%012d", n)
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// writeMarker writes the pending-<name> marker llmux would have written.
// resume=true models a historical session (`--resume <id>`): its transcript
// already exists and its pane may be dead. resume=false models a fresh launch,
// which the relay only pairs while the pane is alive.
func writeMarker(t *testing.T, stateDir, name, target, sessionID string, resume bool) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"target": target, "spawnInstant": time.Now().UnixMilli(), "sessionID": sessionID, "resume": resume,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "pending-"+name), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// writeSessionsJSON pre-populates the relay's session store with one active
// session whose thread already exists.
func writeSessionsJSON(t *testing.T, stateDir, sessionID, target, threadID, channelID string) {
	t.Helper()
	body := fmt.Sprintf(`[{"session_id":%q,"tmux_target":%q,"thread_id":%q,"thread_root_id":"root-%s","channel_id":%q,"status":"active"}]`,
		sessionID, target, threadID, threadID, channelID)
	if err := os.WriteFile(filepath.Join(stateDir, "sessions.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// startFakePane starts fake-claude in a detached tmux session named name and
// returns its pane target. args are fake-claude's arguments (usually
// --session-id <uuid> or --resume <uuid>); env are extra KEY=VALUE pairs.
func startFakePane(t *testing.T, name, projects string, env []string, args ...string) string {
	t.Helper()
	fake := mustBin(t, "fake-claude")
	cmd := []string{"new-session", "-d", "-s", name, "-x", "200", "-y", "50", "env", "RELAY_CLAUDE_PROJECTS=" + projects}
	cmd = append(cmd, env...)
	cmd = append(cmd, fake)
	cmd = append(cmd, args...)
	if out, err := exec.Command("tmux", cmd...).CombinedOutput(); err != nil {
		t.Fatalf("start fake-claude pane %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })
	return name + ":0.0"
}

func capturePane(pane string) string {
	out, _ := exec.Command("tmux", "capture-pane", "-t", pane, "-p").Output()
	return string(out)
}

// waitForPaneReady waits for the idle Claude Code input line.
func waitForPaneReady(t *testing.T, pane string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if inject.PaneReady(capturePane(pane)) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pane %s never became ready; pane:\n%s", pane, capturePane(pane))
}

// submitInPane types text into the pane and presses Enter, as a person would.
func submitInPane(t *testing.T, pane, text string) {
	t.Helper()
	if out, err := exec.Command("tmux", "send-keys", "-t", pane, "-l", text).CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v\n%s", err, out)
	}
	if out, err := exec.Command("tmux", "send-keys", "-t", pane, "Enter").CombinedOutput(); err != nil {
		t.Fatalf("send-keys Enter: %v\n%s", err, out)
	}
}

// ── transcript fixtures ──────────────────────────────────────────────────────

const testCwd = "/work/demo"

// txWriter appends Claude Code transcript lines (the shapes in testdata/claude)
// to <projects>/<slug>/<session-id>.jsonl. The file is created by its first line.
type txWriter struct {
	t    *testing.T
	path string
	sid  string
	last string
	n    int
}

func newTx(t *testing.T, projects, sessionID string) *txWriter {
	t.Helper()
	dir := filepath.Join(projects, "-work-demo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	return &txWriter{t: t, path: filepath.Join(dir, sessionID+".jsonl"), sid: sessionID}
}

func (w *txWriter) line(l map[string]any) {
	w.t.Helper()
	w.n++
	uuid := fmt.Sprintf("00000000-0000-4000-8000-%s%02d", w.sid[len(w.sid)-10:], w.n)
	l["parentUuid"] = nil
	if w.last != "" {
		l["parentUuid"] = w.last
	}
	l["isSidechain"] = false
	l["uuid"] = uuid
	l["timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	l["cwd"] = testCwd
	l["sessionId"] = w.sid
	data, err := json.Marshal(l)
	if err != nil {
		w.t.Fatal(err)
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		w.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		w.t.Fatal(err)
	}
	w.last = uuid
}

func (w *txWriter) User(text string) *txWriter {
	w.line(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	return w
}

func (w *txWriter) assistant(blocks []map[string]any) *txWriter {
	w.line(map[string]any{"type": "assistant", "message": map[string]any{
		"model": "claude-fake-1", "id": fmt.Sprintf("msg_fix_%s_%d", w.sid[len(w.sid)-4:], w.n), "role": "assistant", "content": blocks,
	}})
	return w
}

func (w *txWriter) Text(text string) *txWriter {
	return w.assistant([]map[string]any{{"type": "text", "text": text}})
}

func (w *txWriter) ToolUse(id, name string, input map[string]any) *txWriter {
	return w.assistant([]map[string]any{{"type": "tool_use", "id": id, "name": name, "input": input}})
}

func (w *txWriter) ToolResult(id, content string, isError bool) *txWriter {
	w.line(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError},
	}}})
	return w
}

// TurnEnd ends the turn the way a completed Claude Code turn does.
func (w *txWriter) TurnEnd() *txWriter {
	w.line(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1200, "isMeta": false})
	return w
}

// Interrupt ends the turn the way an interrupt does (no turn_duration follows).
func (w *txWriter) Interrupt() *txWriter {
	w.line(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "[Request interrupted by user for tool use]"},
	}}})
	return w
}

// transcriptPrompts returns the user prompts recorded for a session.
func transcriptPrompts(projects, sessionID string) []string {
	path, err := (transcript.Store{Root: projects}).Path(sessionID)
	if err != nil {
		return nil
	}
	events, _ := transcript.ReadAll(path)
	var out []string
	for _, ev := range events {
		if ev.Kind == transcript.KindUserPrompt {
			out = append(out, ev.Text)
		}
	}
	return out
}

// countPrompts counts the recorded prompts of a session containing needle.
func countPrompts(projects, sessionID, needle string) int {
	n := 0
	for _, p := range transcriptPrompts(projects, sessionID) {
		if strings.Contains(p, needle) {
			n++
		}
	}
	return n
}

// waitForPrompt waits until the session's transcript holds a prompt containing text.
func waitForPrompt(t *testing.T, projects, sessionID, text string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if countPrompts(projects, sessionID, text) > 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("prompt %q not in the transcript of session %s after %s", text, sessionID, timeout)
}

// transcriptSessionIDs lists the session ids that have a transcript.
func transcriptSessionIDs(projects string) []string {
	var ids []string
	files, _ := filepath.Glob(filepath.Join(projects, "*", "*.jsonl"))
	for _, f := range files {
		ids = append(ids, strings.TrimSuffix(filepath.Base(f), ".jsonl"))
	}
	return ids
}

// waitForNewTranscript waits for a transcript whose id is not in known and
// returns that id.
func waitForNewTranscript(t *testing.T, projects string, known []string, timeout time.Duration) string {
	t.Helper()
	skip := map[string]bool{}
	for _, k := range known {
		skip[k] = true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range transcriptSessionIDs(projects) {
			if !skip[id] {
				return id
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no new transcript appeared under %s within %s", projects, timeout)
	return ""
}

// transcriptRaw returns the raw transcript text of a session.
func transcriptRaw(projects, sessionID string) string {
	path, err := (transcript.Store{Root: projects}).Path(sessionID)
	if err != nil {
		return ""
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

// ── smoke tests: the fake itself ─────────────────────────────────────────────

// TestSmoke_FakeClaudeStartsAndReady verifies that fake-claude starts in a tmux
// pane, shows the idle input line, writes no transcript until the first prompt
// is submitted, and then records the exchange in the real JSONL shapes.
func TestSmoke_FakeClaudeStartsAndReady(t *testing.T) {
	mustBin(t, "tmux")
	projects := t.TempDir()
	sid := testSID(1)
	pane := startFakePane(t, fmt.Sprintf("smoke-test-%d", time.Now().UnixMilli()), projects, nil, "--session-id", sid)

	waitForPaneReady(t, pane, 10*time.Second)
	t.Log("✓ idle input line appeared in the pane")

	time.Sleep(500 * time.Millisecond)
	if ids := transcriptSessionIDs(projects); len(ids) != 0 {
		t.Fatalf("transcript exists before the first prompt: %v", ids)
	}
	t.Log("✓ no transcript before the first prompt")

	submitInPane(t, pane, "hello smoke")
	if err := waitForPrompt(t, projects, sid, "hello smoke", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	path, err := (transcript.Store{Root: projects}).Path(sid)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(path)) == "" || !strings.HasSuffix(path, sid+".jsonl") {
		t.Fatalf("unexpected transcript path %s", path)
	}
	deadline := time.Now().Add(10 * time.Second)
	var kinds []transcript.Kind
	for time.Now().Before(deadline) {
		events, _ := transcript.ReadAll(path)
		kinds = kinds[:0]
		for _, ev := range events {
			kinds = append(kinds, ev.Kind)
		}
		if len(events) > 0 && events[len(events)-1].Kind == transcript.KindTurnEnd {
			t.Logf("✓ transcript %s: prompt, assistant text, turn end", path)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("turn did not complete; event kinds: %v", kinds)
}

// TestSmoke_FakeClaudeRefusesUnknownSession verifies that --resume with an
// unknown id prints the real CLI's refusal and exits non-zero.
func TestSmoke_FakeClaudeRefusesUnknownSession(t *testing.T) {
	fake := mustBin(t, "fake-claude")
	bogusID := testSID(999)

	cmd := exec.Command(fake, "--resume", bogusID)
	cmd.Env = append(os.Environ(), "RELAY_CLAUDE_PROJECTS="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown session, got nil error")
	}
	if !strings.Contains(string(out), "No conversation found with session ID: "+bogusID) {
		t.Errorf("expected refusal text in output, got:\n%s", out)
	}
	t.Logf("✓ refusal text correct: %s", strings.TrimSpace(string(out)))
}

// TestSmoke_FakeClaudeRefuseFirstResume verifies FAKE_CLAUDE_REFUSE_FIRST_RESUME=1
// refuses a resume even when the session's transcript exists.
func TestSmoke_FakeClaudeRefuseFirstResume(t *testing.T) {
	fake := mustBin(t, "fake-claude")
	projects := t.TempDir()
	sid := testSID(2)
	newTx(t, projects, sid).User("hi").Text("hello").TurnEnd()

	cmd := exec.Command(fake, "--resume", sid)
	cmd.Env = append(os.Environ(), "RELAY_CLAUDE_PROJECTS="+projects, "FAKE_CLAUDE_REFUSE_FIRST_RESUME=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "No conversation found with session ID:") {
		t.Errorf("expected refusal text, got:\n%s", out)
	}
	t.Log("✓ FAKE_CLAUDE_REFUSE_FIRST_RESUME works")
}

// TestSmoke_FakeClaudeTrustDialog verifies FAKE_CLAUDE_UNTRUSTED=1 shows the
// folder-trust dialog first and that Down + Enter trusts the folder.
func TestSmoke_FakeClaudeTrustDialog(t *testing.T) {
	mustBin(t, "tmux")
	projects := t.TempDir()
	pane := startFakePane(t, fmt.Sprintf("smoke-trust-%d", time.Now().UnixMilli()), projects,
		[]string{"FAKE_CLAUDE_UNTRUSTED=1"}, "--session-id", testSID(3))

	if err := waitForPaneText(t, pane, "Quick safety check", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if inject.PaneReady(capturePane(pane)) {
		t.Fatal("pane reported ready while the trust dialog is showing")
	}
	for _, key := range []string{"Down", "Enter"} {
		if out, err := exec.Command("tmux", "send-keys", "-t", pane, key).CombinedOutput(); err != nil {
			t.Fatalf("send-keys %s: %v\n%s", key, err, out)
		}
	}
	waitForPaneReady(t, pane, 10*time.Second)
	t.Log("✓ trust dialog shown, then accepted with Down+Enter")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func waitForPaneText(t *testing.T, pane, substr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("tmux", "capture-pane", "-t", pane, "-p")
		out, err := cmd.Output()
		if err == nil && strings.Contains(string(out), substr) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Last capture for diagnostics
	cmd := exec.Command("tmux", "capture-pane", "-t", pane, "-p")
	out, _ := cmd.Output()
	return fmt.Errorf("timeout after %s waiting for %q; pane content:\n%s", timeout, substr, out)
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("file %s did not appear within %s", path, timeout)
}
