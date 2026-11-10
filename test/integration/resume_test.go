//go:build integration

// resume_test.go: integration tests for milestone 4 (resume dead sessions,
// retry refusals, verify injection echo).
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── TestInvariant3_InjectionEchoVerification ─────────────────────────────────
//
// Invariant 3: after the relay injects a prompt into the pane, the echo must
// appear in the transcript within 30 s; if absent on the first inject, the
// relay re-sends once, the echo appears after the retry, and the Discord
// thread shows the "You:" line exactly once (duplicate-echo suppression).
//
// FAKE_CLAUDE_DROP_FIRST_PROMPT=1 makes the pane lose the first prompt it is
// given (nothing shown, nothing recorded), so the relay's echo-verify loop
// must notice the absent echo and retry.
func TestInvariant3_InjectionEchoVerification(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	// Keep this long-running retry test on its own tmux server. A stale llm-*
	// session on the shared default socket can otherwise satisfy a readiness
	// probe while the pane under test dies.
	tmuxTmpDir := tmuxIsolatedSocketDir(t)

	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19894", "19895"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(40)
	seedHistory(t, projects, sessionID)

	sessionName := "llm-echo-verify"
	paneCmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-x", "200", "-y", "50",
		"env", "RELAY_CLAUDE_PROJECTS="+projects, "FAKE_CLAUDE_DROP_FIRST_PROMPT=1",
		mustBin(t, "fake-claude"), "--resume", sessionID)
	paneCmd.Env = append(envWithoutTmux(), fmt.Sprintf("TMUX_TMPDIR=%s", tmuxTmpDir))
	if out, err := paneCmd.CombinedOutput(); err != nil {
		t.Fatalf("start fake-claude tmux: %v\n%s", err, out)
	}
	paneTarget := sessionName + ":0.0"
	writeMarker(t, stateDir, sessionName, paneTarget, sessionID, true)

	startRelay(t, relayBin, projects, stateDir, mockPort, "DISCORD_USER_ID=user-echo-001", "DISCORD_CHANNEL_ID=test-echo-ch",
		"TMUX=", "TMUX_TMPDIR="+tmuxTmpDir)

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 15*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)
	if err := waitForSessionRecord(stateDir, sessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	clearSentMessages(mockPort)

	// The relay waits for the idle input line, injects (the pane drops it),
	// waits up to 30 s for the echo in the transcript, re-sends once, and the
	// retry lands.
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-echo-001", threadID, "user-echo-001", false, "echo verification probe", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Up to 45 s to cover the 30 s verify window.
	if err := waitForPrompt(t, projects, sessionID, "echo verification probe", 45*time.Second); err != nil {
		t.Fatalf("echo not found in the transcript: %v", err)
	}
	t.Log("✓ Invariant 3: injection echo appeared in the transcript after the retry")
	if n := countPrompts(projects, sessionID, "echo verification probe"); n != 1 {
		t.Errorf("expected exactly 1 recorded prompt (first was dropped, retry landed), got %d", n)
	}

	// Allow a brief settling window for the relay to post after the echo appears.
	deadline := time.Now().Add(10 * time.Second)
	youCount := 0
	for time.Now().Before(deadline) {
		youCount = 0
		msgs, _ := getSentMessages(mockPort)
		for _, m := range msgs {
			c := contentOf(m)
			if strings.Contains(c, "You:") && strings.Contains(c, "echo verification probe") {
				youCount++
			}
		}
		if youCount > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)
	msgs, _ := getSentMessages(mockPort)
	youCount = 0
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, "You:") && strings.Contains(c, "echo verification probe") {
			youCount++
		}
	}
	if youCount != 1 {
		t.Errorf("expected exactly 1 'You: echo verification probe' in Discord thread, got %d", youCount)
	}
	t.Logf("✓ Invariant 3: Discord echo count = %d (expected 1)", youCount)
}

// resumeWrapper writes a stand-in for llmux that counts its calls and, for the
// first refuseCalls calls, makes the launched fake-claude refuse the resume
// (FAKE_CLAUDE_REFUSE_FIRST_RESUME=1). Every launch is the real llmux.
func resumeWrapper(t *testing.T, refuseCalls int) (script, countFile string) {
	t.Helper()
	llmux := mustBin(t, "llmux")
	dir := t.TempDir()
	countFile = filepath.Join(dir, "call-count")
	if err := os.WriteFile(countFile, []byte("0"), 0644); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "llmux-wrapper")
	body := fmt.Sprintf(`#!/bin/sh
# Counts llmux calls; the first %d resume launches are refused by the pane.
count=$(cat %q 2>/dev/null || echo 0)
count=$((count + 1))
printf '%%s' "$count" > %q
if [ "$count" -le %d ]; then
  export FAKE_CLAUDE_REFUSE_FIRST_RESUME=1
  export FAKE_CLAUDE_REFUSE_LINGER_MS=1500
fi
exec %q "$@"
`, refuseCalls, countFile, countFile, refuseCalls, llmux)
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return script, countFile
}

// tmuxServerCleanup kills llm-* sessions and the server on one test's private
// tmux socket namespace; it never touches another test's or the host's server.
func tmuxServerCleanup(t *testing.T, tmuxTmpDir string) {
	t.Helper()
	t.Cleanup(func() {
		sock := filepath.Join(tmuxTmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
		exec.Command("tmux", "-S", sock, "kill-server").Run()
	})
}

// seedDeadSession writes the transcript of a finished session, a resume marker
// for a pane that no longer exists, and returns after the relay has mirrored it.
func seedDeadSession(t *testing.T, projects, stateDir, sessionName, sessionID, text string) {
	t.Helper()
	newTx(t, projects, sessionID).User("earlier question").Text(text).TurnEnd()
	writeMarker(t, stateDir, sessionName, sessionName+":0.0", sessionID, true)
}

// ── TestInvariant4_ResumeRefusalRetry ────────────────────────────────────────
//
// Invariant 4a: when a thread reply arrives for a session whose pane is dead,
// the relay resumes it with `llmux --resume <id>`. If the first attempt is
// refused ("No conversation found with session ID"), the relay tears the
// refused pane down, retries the same resume silently, and injects the queued
// prompt into the resumed session once the retry succeeds.
func TestInvariant4_ResumeRefusalRetry(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	fakeClaude := mustBin(t, "fake-claude")
	// Isolated tmux socket namespace for this test only: every tmux invocation
	// (the relay's own, and the llmux processes it spawns, which inherit the
	// relay's environment) resolves the same default socket under this dir.
	tmuxTmpDir := tmuxIsolatedSocketDir(t)
	tmuxServerCleanup(t, tmuxTmpDir)

	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19896", "19897"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(41)
	seedDeadSession(t, projects, stateDir, "llm-refusal-test", sessionID, "Ready for refusal test")
	wrapper, countFile := resumeWrapper(t, 1)

	startRelay(t, relayBin, projects, stateDir, mockPort,
		"DISCORD_USER_ID=user-refusal-001", "DISCORD_CHANNEL_ID=test-refusal-ch",
		"RELAY_LLMUX_BIN="+wrapper, "RELAY_AGENT_CMD="+fakeClaude,
		"TMUX=", "TMUX_TMPDIR="+tmuxTmpDir)

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 30*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)
	time.Sleep(500 * time.Millisecond)

	// The pane is dead: resume → refusal (attempt 1) → teardown → resume (attempt 2) → success.
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-refusal-001", threadID, "user-refusal-001", false, "refusal retry test prompt", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if err := waitForPrompt(t, projects, sessionID, "refusal retry test prompt", 60*time.Second); err != nil {
		t.Fatalf("prompt not found after refusal retry: %v", err)
	}
	t.Log("✓ Invariant 4a: resume refusal detected, retried, prompt injected into the SAME session")

	count := strings.TrimSpace(mustRead(t, countFile))
	if count != "2" {
		t.Fatalf("expected exactly 2 resume launches (refusal + retry), got %q", count)
	}
	if ids := transcriptSessionIDs(projects); len(ids) != 1 {
		t.Fatalf("a retry that succeeded must not start another session; transcripts: %v", ids)
	}
	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		if strings.Contains(contentOf(m), "fresh session") {
			t.Fatalf("a single refusal must be retried silently, not announced as a fresh session: %q", contentOf(m))
		}
	}
	t.Logf("llmux call count: %s (expected exactly 2)", count)
}

// ── TestInvariant4_DoubleRefusalStartsFreshSession ───────────────────────────
//
// Invariant 4a (second half): when the resume is refused twice, the relay tears
// both panes down, announces ONE notice in the thread, and starts a brand-new
// session (a new session id) that receives the queued prompt. The old
// transcript never receives it.
func TestInvariant4_DoubleRefusalStartsFreshSession(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	fakeClaude := mustBin(t, "fake-claude")
	tmuxTmpDir := tmuxIsolatedSocketDir(t)
	tmuxServerCleanup(t, tmuxTmpDir)

	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19954", "19955"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(42)
	seedDeadSession(t, projects, stateDir, "llm-double-refusal", sessionID, "Ready for double refusal")
	wrapper, countFile := resumeWrapper(t, 2)

	startRelay(t, relayBin, projects, stateDir, mockPort,
		"DISCORD_USER_ID=user-refusal-002", "DISCORD_CHANNEL_ID=test-refusal-ch2",
		"RELAY_LLMUX_BIN="+wrapper, "RELAY_AGENT_CMD="+fakeClaude,
		"TMUX=", "TMUX_TMPDIR="+tmuxTmpDir)

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 30*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-refusal-002", threadID, "user-refusal-002", false, "double refusal prompt", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if err := waitForMessageContent(t, mockPort, "fresh session", 60*time.Second); err != nil {
		t.Fatalf("fresh-session notice not posted: %v", err)
	}
	fresh := waitForNewTranscript(t, projects, []string{sessionID}, 60*time.Second)
	if err := waitForPrompt(t, projects, fresh, "double refusal prompt", 30*time.Second); err != nil {
		t.Fatalf("queued prompt never reached the fresh session: %v", err)
	}
	if n := countPrompts(projects, sessionID, "double refusal prompt"); n != 0 {
		t.Fatalf("the refused session received the prompt %d time(s)", n)
	}
	time.Sleep(1 * time.Second)
	notices := 0
	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		if strings.Contains(contentOf(m), "fresh session") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("expected exactly one fresh-session notice, got %d", notices)
	}
	if count := strings.TrimSpace(mustRead(t, countFile)); count != "3" {
		t.Fatalf("expected 3 llmux launches (two refused resumes + one fresh session), got %q", count)
	}
	t.Logf("✓ Invariant 4a: double refusal → one notice → fresh session %s got the prompt", fresh)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ── TestInvariant4_AmbiguousIdCandidateList ───────────────────────────────────
//
// Invariant 4b: when `!resume` is issued without a session number and several
// candidate sessions exist, the relay posts a numbered candidate list including
// each session's model and last-active age. Replying `!resume 1` attempts the
// resume of the first candidate.
//
// sessions.json is pre-populated with two sessions (dead tmux targets, known
// thread ids) and both have transcripts, so the relay already knows both
// threads and never needs a pane to be created concurrently.
func TestInvariant4_AmbiguousIdCandidateList(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	// Private tmux namespace: `!resume 1` really launches llmux.
	tmuxTmpDir := tmuxIsolatedSocketDir(t)
	tmuxServerCleanup(t, tmuxTmpDir)

	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19898", "19899"
	setupMock(t, mockPort, mockGWPort)

	sessionA, sessionB := testSID(43), testSID(44)
	newTx(t, projects, sessionA).User("question A").Text("answer A").TurnEnd()
	newTx(t, projects, sessionB).User("question B").Text("answer B").TurnEnd()
	// A is older than B.
	old := time.Now().Add(-1 * time.Hour)
	for _, id := range []string{sessionA} {
		p, _ := filepath.Glob(filepath.Join(projects, "*", id+".jsonl"))
		_ = os.Chtimes(p[0], old, old)
	}

	threadA, threadB := "ch-ambig-threadA-001", "ch-ambig-threadB-001"
	channelID := "ch-ambig-001"
	sessionsData := fmt.Sprintf(`[
  {"session_id": %q, "tmux_target": "llm-dead-A:0.0", "thread_id": %q, "thread_root_id": "root-A-001", "channel_id": %q, "status": "active"},
  {"session_id": %q, "tmux_target": "llm-dead-B:0.0", "thread_id": %q, "thread_root_id": "root-B-001", "channel_id": %q, "status": "active"}
]`, sessionA, threadA, channelID, sessionB, threadB, channelID)
	if err := os.WriteFile(filepath.Join(stateDir, "sessions.json"), []byte(sessionsData), 0644); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	relayCmd := startRelay(t, relayBin, projects, stateDir, mockPort,
		"DISCORD_USER_ID=user-ambig-001", "DISCORD_CHANNEL_ID="+channelID,
		"RELAY_AGENT_CMD="+mustBin(t, "fake-claude"), "TMUX=", "TMUX_TMPDIR="+tmuxTmpDir)

	waitForRelayReady(t, stateDir)
	clearSentMessages(mockPort)

	// Bare `!resume` in threadA: the relay finds two sessions and posts a numbered list.
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-ambig-resume", threadA, "user-ambig-001", false, "!resume", nil)); err != nil {
		t.Fatalf("dispatch !resume: %v", err)
	}
	if err := waitForMessageContent(t, mockPort, "1.", 15*time.Second); err != nil {
		t.Fatalf("candidate list not posted: %v", err)
	}
	t.Log("✓ Invariant 4b: candidate list posted for ambiguous !resume")

	msgs, _ := getSentMessages(mockPort)
	var candidateMsg string
	for _, m := range msgs {
		if c := contentOf(m); strings.Contains(c, "1.") {
			candidateMsg = c
			break
		}
	}
	for _, want := range []string{"model:", "last active:", sessionA, sessionB, "2."} {
		if !strings.Contains(candidateMsg, want) {
			t.Errorf("candidate list missing %q; got: %q", want, candidateMsg)
		}
	}
	// Most recently active first: B (just written) before A (an hour old).
	if ia, ib := strings.Index(candidateMsg, sessionA), strings.Index(candidateMsg, sessionB); ia >= 0 && ib >= 0 && ib > ia {
		t.Errorf("candidates not ordered most-recent first: %q", candidateMsg)
	}

	// Reply `!resume 1` to select the first candidate; the relay resumes it.
	clearSentMessages(mockPort)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-ambig-choice", threadA, "user-ambig-001", false, "!resume 1", nil)); err != nil {
		t.Fatalf("dispatch !resume 1: %v", err)
	}
	time.Sleep(3 * time.Second)
	if relayCmd.ProcessState != nil {
		t.Error("relay process exited unexpectedly after !resume 1")
	}
	msgs, _ = getSentMessages(mockPort)
	for _, m := range msgs {
		t.Logf("  msg: %s", contentOf(m))
	}
	t.Log("✓ Invariant 4b: ambiguous !resume handled without crash")
}

// ── tmux socket isolation helpers ─────────────────────────────────────────
//
// TestInvariant4_ResumeRefusalRetry must never depend on (or leak into)
// another test's tmux server, and must never rely on the host having a
// pre-existing default-socket tmux server running. tmuxIsolatedSocketDir
// gives the test its own private TMUX_TMPDIR; every process the test starts
// (directly, or transitively — the relay binary, and the llmux wrapper
// the relay spawns as a subprocess with an inherited environment) resolves
// tmux's "default" socket underneath this one directory, and nothing else on
// the host ever touches it.

// tmuxIsolatedSocketDir creates a fresh, per-test TMUX_TMPDIR and registers
// cleanup that kills only the tmux server on that socket (never the host's
// default server or any other test's socket).
func tmuxIsolatedSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		killCmd := exec.Command("tmux", "kill-server")
		killCmd.Env = append(envWithoutTmux(), fmt.Sprintf("TMUX_TMPDIR=%s", dir))
		killCmd.Run() //nolint:errcheck // no server running is not an error here
	})
	return dir
}

// envWithoutTmux returns os.Environ() with any inherited TMUX and
// TMUX_TMPDIR stripped, so callers can set their own isolated values without
// an ambient value from the calling shell (this test process itself may be
// running inside a tmux pane) leaking through.
func envWithoutTmux() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_TMPDIR=") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "TMUX=")
	return out
}
