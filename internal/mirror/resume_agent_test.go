package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
)

// fakeResumeLlmux installs a fake llmux that records the RELAY_AGENT_CMD of its
// environment ("<unset>" when absent) in envFile and writes a resume marker that
// carries it, like the real llmux. paneScript, if non-empty, is run in a tmux
// session named after the llmux session.
func fakeResumeLlmux(t *testing.T, w *Watcher, name, paneScript string) (envFile string) {
	t.Helper()
	envFile = filepath.Join(t.TempDir(), "agentenv")
	tmuxLine := ""
	if paneScript != "" {
		tmuxLine = fmt.Sprintf("tmux new-session -d -s %s -x 100 -y 30 sh -c %q\n", name, paneScript)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ -n "${RELAY_AGENT_CMD+x}" ]; then printf '%%s' "$RELAY_AGENT_CMD" > %s; else printf '<unset>' > %s; fi
printf '{"target":"%s:0.0","spawnInstant":1,"sessionID":"%s","resume":true,"owner":"%%s","agentCmd":"%%s"}' "$RELAY_MARKER_OWNER" "$RELAY_AGENT_CMD" > "$RELAY_STATE_DIR/pending-%s"
%secho %s
`, envFile, envFile, name, testSessionID, name, tmuxLine, name)
	bin := filepath.Join(t.TempDir(), "llmux")
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	w.cfg.LlmuxBin = bin
	return envFile
}

func readFileStr(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSessionRecordAgentCmdJSON(t *testing.T) {
	old := `[{"session_id":"s","tmux_target":"t","thread_id":"th","status":"active"}]`
	var list []SessionRecord
	if err := json.Unmarshal([]byte(old), &list); err != nil || list[0].AgentCmd != "" {
		t.Fatalf("old record: %+v err=%v", list, err)
	}
	b, _ := json.Marshal(SessionRecord{SessionID: "s"})
	if strings.Contains(string(b), "agent_cmd") {
		t.Fatalf("empty agent command must be omitted: %s", b)
	}
	b, _ = json.Marshal(SessionRecord{SessionID: "s", AgentCmd: "pl"})
	if !strings.Contains(string(b), `"agent_cmd":"pl"`) {
		t.Fatalf("agent command not serialized: %s", b)
	}
}

func TestScanSessionsStoresMarkerAgentCmd(t *testing.T) {
	root := t.TempDir()
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, root)
	data, _ := json.Marshal(spawnMarkerJSON{Target: "nosuch-agent:0.0", SessionID: testSessionID, Resume: true, AgentCmd: "pl llmux"})
	if err := os.WriteFile(filepath.Join(w.cfg.StateDir, "pending-agent"), data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "-p")
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte(`{"type":"user","uuid":"u","message":{"role":"user","content":"x"}}`+"\n"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { cancel(); waitSessionEnded(w, testSessionID) })
	w.scanSessions(ctx, map[string]bool{})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// The record is rewritten as "ended" once the dead pane is noticed; the
		// agent command must survive that too.
		if rec := w.sessions.Get(testSessionID); rec != nil && rec.ThreadID != "" {
			time.Sleep(300 * time.Millisecond)
			rec = w.sessions.Get(testSessionID)
			if rec.AgentCmd != "pl llmux" {
				t.Fatalf("record agent command = %q, want %q", rec.AgentCmd, "pl llmux")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session was never bound")
}

func TestSpawnResumePassesStoredAgentCmd(t *testing.T) {
	w := newTestWatcher(t, newRecordingClient(), t.TempDir())
	envFile := fakeResumeLlmux(t, w, "llm-agent-a", "")
	t.Setenv("RELAY_AGENT_CMD", "service-agent")
	target, agent, err := w.spawnResume(context.Background(), testSessionID, "pl llmux")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFileStr(t, envFile); got != "pl llmux" {
		t.Fatalf("llmux RELAY_AGENT_CMD = %q, want the stored value", got)
	}
	if target != "llm-agent-a:0.0" || agent != "pl llmux" {
		t.Fatalf("target=%q agent=%q", target, agent)
	}
}

func TestSpawnResumeWithoutStoredAgentCmdLeavesEnvironment(t *testing.T) {
	w := newTestWatcher(t, newRecordingClient(), t.TempDir())
	envFile := fakeResumeLlmux(t, w, "llm-agent-b", "")
	os.Unsetenv("RELAY_AGENT_CMD")
	if _, agent, err := w.spawnResume(context.Background(), testSessionID, ""); err != nil || agent != "" {
		t.Fatalf("agent=%q err=%v", agent, err)
	}
	if got := readFileStr(t, envFile); got != "<unset>" {
		t.Fatalf("RELAY_AGENT_CMD must stay unset, got %q", got)
	}
	t.Setenv("RELAY_AGENT_CMD", "service-agent")
	if _, _, err := w.spawnResume(context.Background(), testSessionID, ""); err != nil {
		t.Fatal(err)
	}
	if got := readFileStr(t, envFile); got != "service-agent" {
		t.Fatalf("service value must pass through as today, got %q", got)
	}
}

// TestResumeDeadSessionMarksActiveAndKeepsAgent: a resumed session is stored as
// active (not as the "ended" of its dead pane) and keeps its agent command.
func TestResumeDeadSessionMarksActiveAndKeepsAgent(t *testing.T) {
	name := fmt.Sprintf("llm-tresume-%d", os.Getpid())
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", name).Run() })
	pane := fmt.Sprintf(`printf '%%s\n' ' Claude Code' '' '%s' '❯ ' '%s' '  ? for shortcuts'; sleep 30`, testRule, testRule)
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.runCtx = ctx
	w.injector = inject.New(w.cfg.StateDir)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	envFile := fakeResumeLlmux(t, w, name, pane)
	os.Unsetenv("RELAY_AGENT_CMD")
	rec := &SessionRecord{SessionID: testSessionID, TmuxTarget: "gone:0.0", ThreadID: "thread-1", ChannelID: "chan", Status: "ended", AgentCmd: "pl llmux"}
	if err := w.sessions.Upsert(rec); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithTimeout(ctx, 4*time.Second) // injection is not under test
	defer rcancel()
	_ = w.resumeDeadSession(rctx, rec, "hello", nil)
	got := w.sessions.Get(testSessionID)
	if got == nil || got.Status != "active" || got.TmuxTarget != name+":0.0" {
		t.Fatalf("record after resume = %+v, want active on the new target", got)
	}
	if got.AgentCmd != "pl llmux" {
		t.Fatalf("resume erased the agent command: %q", got.AgentCmd)
	}
	if v := readFileStr(t, envFile); v != "pl llmux" {
		t.Fatalf("llmux RELAY_AGENT_CMD = %q", v)
	}
}
