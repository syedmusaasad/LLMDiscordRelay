//go:build integration

// spawn_test.go: integration tests for milestone 3 (spawn a Claude Code session
// from a Discord channel message) and the fail-closed binding contract
// (docs/BINDING-PROTOCOL.md): llmux chooses the session id, so a transcript with
// exactly that id is the only thing that can ever be bound.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listLlmSessions returns the names of all tmux sessions starting with "llm-".
func listLlmSessions() []string {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, s := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(s, "llm-") {
			names = append(names, s)
		}
	}
	return names
}

func killLlmSessionsCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, s := range listLlmSessions() {
			exec.Command("tmux", "kill-session", "-t", "="+s).Run()
		}
	})
}

// spawnRelay starts a relay that spawns fake-claude sessions through the real
// llmux (RELAY_AGENT_CMD points at fake-claude).
func spawnRelay(t *testing.T, projects, stateDir, mockPort, channel, user string, extra ...string) {
	t.Helper()
	env := []string{
		"DISCORD_CHANNEL_ID=" + channel,
		"DISCORD_USER_ID=" + user,
		"RELAY_AGENT_CMD=" + mustBin(t, "fake-claude"),
	}
	startRelay(t, mustBin(t, "relay"), projects, stateDir, mockPort, append(env, extra...)...)
}

// threadSessionIDs returns every session id mentioned in the sent messages
// (thread root, thread name) other than the ones in ignore.
func sentSessionIDs(mockPort string, ignore ...string) []string {
	skip := map[string]bool{}
	for _, i := range ignore {
		skip[i] = true
	}
	seen := map[string]bool{}
	var out []string
	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		for _, id := range uuidRe.FindAllString(string(m), -1) {
			if !skip[id] && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// ── TestInvariant1_OwnTranscriptBinding ──────────────────────────────────────
//
// Invariant 1: a stale transcript that appears AFTER relay start but BEFORE the
// spawn must never be bound to the spawn, even though it is a perfectly valid
// session of the same project.
//
// The spawn's identity is the id llmux generated (claude --session-id <id>);
// nothing is inferred from timing or ordering.
func TestInvariant1_OwnTranscriptBinding(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19890", "19891"
	setupMock(t, mockPort, mockGWPort)

	spawnRelay(t, projects, stateDir, mockPort, "test-spawn-ch1", "user-inv1-001")

	time.Sleep(1200 * time.Millisecond)

	// A stale session of the same project, written after relay start.
	staleID := testSID(50)
	newTx(t, projects, staleID).User("an unrelated earlier session").Text("stale reply").TurnEnd()
	t.Logf("stale session written: id=%s (valid, same project, after relay start)", staleID)
	time.Sleep(1200 * time.Millisecond)

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-inv1-trigger", "test-spawn-ch1", "user-inv1-001", false,
		"invariant1: this is the first prompt", nil)); err != nil {
		t.Fatalf("dispatch channel message: %v", err)
	}

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 60*time.Second); err != nil {
		t.Fatalf("thread not created within 60s: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		if c := contentOf(m); strings.Contains(c, staleID) || strings.Contains(c, "stale reply") {
			t.Errorf("stale session appeared in Discord message %q; the spawn bound the wrong session", c)
		}
	}
	newID := waitForNewTranscript(t, projects, []string{staleID}, 30*time.Second)
	if newID == staleID {
		t.Fatal("spawn reused the stale session")
	}
	if err := waitForPrompt(t, projects, newID, "invariant1: this is the first prompt", 30*time.Second); err != nil {
		t.Fatalf("first prompt not injected into the new session: %v", err)
	}
	if n := countPrompts(projects, staleID, "invariant1"); n != 0 {
		t.Errorf("first prompt was injected into the stale session")
	}
	if err := waitForMessageContent(t, mockPort, newID, 10*time.Second); err != nil {
		t.Errorf("Discord messages never name the spawned session %s: %v", newID, err)
	}
	t.Log("✓ Invariant 1: stale session not bound; the llmux-chosen session id is the one mirrored")
}

// ── TestSpawn_ChannelMessageTriggersLlmux ────────────────────────────────────
//
// A Discord message in the relay channel (not a thread) makes the relay spawn
// llmux, create exactly one Discord thread for the new session, and inject the
// message text as the first prompt.
func TestSpawn_ChannelMessageTriggersLlmux(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19892", "19893"
	setupMock(t, mockPort, mockGWPort)

	spawnRelay(t, projects, stateDir, mockPort, "test-spawn-ch2", "user-spawn-002")
	time.Sleep(300 * time.Millisecond)

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-ch-spawn-002", "test-spawn-ch2", "user-spawn-002", false,
		"please do something useful for me", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 60*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("✓ thread created: id=%s", threadID)

	sessionID := waitForNewTranscript(t, projects, nil, 30*time.Second)
	if err := waitForPrompt(t, projects, sessionID, "please do something useful for me", 30*time.Second); err != nil {
		t.Fatalf("first prompt not injected: %v", err)
	}
	t.Logf("✓ first prompt injected into the spawned session %s", sessionID)

	// One thread per session: exactly one thread was created for the spawn.
	time.Sleep(2 * time.Second)
	if n := countThreads(mockPort); n != 1 {
		t.Fatalf("expected exactly one thread for one session, got %d", n)
	}
	if err := waitForSessionRecord(stateDir, sessionID, 10*time.Second); err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
}

// countThreads counts CreateThread requests (type 11) the mock saw.
func countThreads(mockPort string) int {
	n := 0
	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		var body struct {
			Type int `json:"type"`
		}
		if json.Unmarshal(m, &body) == nil && body.Type == 11 {
			n++
		}
	}
	return n
}

// ── TestInvariant1_ConcurrentForeignSessionRejected ──────────────────────────
//
// Invariant 1: an unrelated, foreign transcript that appears while the real
// spawn's own transcript is still delayed — including one in the exact same
// project directory — must never be bound. Only the session whose id llmux
// generated is bound and receives the queued prompt.
func TestInvariant1_ConcurrentForeignSessionRejected(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19956", "19957"
	setupMock(t, mockPort, mockGWPort)

	// fake-claude delays creating its own transcript by 2s after the prompt is
	// submitted, opening a window in which the foreign transcript appears.
	spawnRelay(t, projects, stateDir, mockPort, "test-spawn-ch3", "user-inv1c-001", "FAKE_CLAUDE_TRANSCRIPT_DELAY_MS=2000")
	time.Sleep(300 * time.Millisecond)

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-inv1c-trigger", "test-spawn-ch3", "user-inv1c-001", false,
		"invariant1c: this is the first prompt", nil)); err != nil {
		t.Fatalf("dispatch channel message: %v", err)
	}

	// Give llmux time to launch and write the pending marker, then write the
	// foreign session BEFORE the fake's own delayed transcript appears.
	time.Sleep(600 * time.Millisecond)
	foreignID := testSID(51)
	if ids := transcriptSessionIDs(projects); len(ids) != 0 {
		t.Fatalf("real transcript already exists (%v); the delay window is gone", ids)
	}
	newTx(t, projects, foreignID).User("a concurrent foreign session").Text("foreign reply").TurnEnd()

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 60*time.Second); err != nil {
		t.Fatalf("thread not created within 60s: %v", err)
	}

	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		if c := contentOf(m); strings.Contains(c, foreignID) || strings.Contains(c, "foreign reply") {
			t.Errorf("foreign session appeared in Discord message %q; the spawn bound the wrong session", c)
		}
	}
	boundID := waitForNewTranscript(t, projects, []string{foreignID}, 30*time.Second)
	if err := waitForPrompt(t, projects, boundID, "invariant1c: this is the first prompt", 30*time.Second); err != nil {
		t.Fatalf("first prompt not injected into the bound session: %v", err)
	}
	if n := countPrompts(projects, foreignID, "invariant1c"); n != 0 {
		t.Errorf("prompt was injected into the foreign session; expected only the real session")
	}
	if err := waitForMessageContent(t, mockPort, boundID, 10*time.Second); err != nil {
		t.Errorf("Discord messages never name the bound session %s: %v", boundID, err)
	}
	t.Log("✓ Invariant 1c: foreign concurrent session rejected; the prompt landed only in the real session")
}

// ── TestInvariant1_MissingIdentityFailsClosed ────────────────────────────────
//
// Invariant 1 (fail closed): when the launched process never produces a
// transcript for its id (here it exits right away, so the pane dies during the
// wait) the relay must NEVER claim, mirror or inject into anything, even when
// an eligible foreign transcript exists and the pane was briefly alive.
func TestInvariant1_MissingIdentityFailsClosed(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19900", "19901"
	setupMock(t, mockPort, mockGWPort)

	// Stub agent: starts (pane briefly alive), writes nothing, exits.
	stub := filepath.Join(t.TempDir(), "stub-no-transcript")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 0.3\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	startRelay(t, mustBin(t, "relay"), projects, stateDir, mockPort,
		"DISCORD_CHANNEL_ID=test-spawn-ch4", "DISCORD_USER_ID=user-inv1m-001",
		"RELAY_AGENT_CMD="+stub, "RELAY_SPAWN_BIND_TIMEOUT_MS=3000")
	time.Sleep(300 * time.Millisecond)

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-inv1m-trigger", "test-spawn-ch4", "user-inv1m-001", false,
		"invariant1m: should never be injected", nil)); err != nil {
		t.Fatalf("dispatch channel message: %v", err)
	}

	// While the bind wait is in progress, an eligible foreign session appears.
	time.Sleep(500 * time.Millisecond)
	foreignID := testSID(52)
	newTx(t, projects, foreignID).User("eligible but unproven").Text("unproven reply").TurnEnd()

	// Wait past the bind timeout, then assert nothing was ever bound.
	time.Sleep(5 * time.Second)

	msgs, _ := getSentMessages(mockPort)
	if len(msgs) != 0 {
		contents := make([]string, 0, len(msgs))
		for _, m := range msgs {
			contents = append(contents, contentOf(m))
		}
		t.Fatalf("expected no Discord messages/threads when no transcript proves the identity, got %d: %v", len(msgs), contents)
	}
	t.Log("✓ Invariant 1: no proven identity → no thread/message ever created")

	if n := countPrompts(projects, foreignID, "invariant1m"); n != 0 {
		t.Errorf("prompt text was injected into the foreign session")
	}
	if ids := transcriptSessionIDs(projects); len(ids) != 1 {
		t.Errorf("unexpected transcripts: %v", ids)
	}
	t.Log("✓ no injection occurred without a verified identity")
}

// ── TestSpawn_PromptBeforeBind ───────────────────────────────────────────────
//
// Claude Code writes its transcript only when the first prompt is submitted, so
// the relay must deliver the prompt first and bind afterwards; binding before
// injecting would deadlock until the bind timeout and never create a thread.
func TestSpawn_PromptBeforeBind(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()

	const prompt = "prompt-before-bind: hello from discord"
	const reply = "PBB-REPLY-MIRRORED"
	script := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(`[{"prompt":%q,"reply":%q}]`, prompt, reply)), 0644); err != nil {
		t.Fatal(err)
	}

	mockPort, mockGWPort := "19940", "19941"
	setupMock(t, mockPort, mockGWPort)
	spawnRelay(t, projects, stateDir, mockPort, "test-spawn-pbb", "user-pbb-001",
		"FAKE_CLAUDE_SCRIPT="+script, "RELAY_SPAWN_BIND_TIMEOUT_MS=15000")

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-pbb-1", "test-spawn-pbb", "user-pbb-001", false, prompt, nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// The prompt is visible in the pane even though no transcript exists yet.
	var paneTarget string
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) && paneTarget == "" {
		for _, s := range listLlmSessions() {
			if strings.Contains(capturePane(s+":0.0"), prompt) {
				paneTarget = s + ":0.0"
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if paneTarget == "" {
		t.Fatalf("first prompt never appeared in a spawned pane (deadlock: prompt withheld until identity)")
	}
	t.Logf("✓ prompt visible in pane %s", paneTarget)

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 40*time.Second); err != nil {
		t.Fatalf("thread not created after prompt-first delivery: %v", err)
	}
	t.Logf("✓ thread created: id=%s", threadID)

	sessionID := waitForNewTranscript(t, projects, nil, 30*time.Second)
	if err := waitForPrompt(t, projects, sessionID, prompt, 30*time.Second); err != nil {
		t.Fatalf("prompt never landed in the transcript: %v", err)
	}
	t.Logf("✓ prompt in the transcript of %s", sessionID)

	if err := waitForMessageContent(t, mockPort, reply, 30*time.Second); err != nil {
		t.Fatalf("reply not mirrored: %v", err)
	}
	if err := waitForMessageContent(t, mockPort, sessionID, 5*time.Second); err != nil {
		t.Fatalf("root message does not name the bound session: %v", err)
	}
	if err := waitForSessionRecord(stateDir, sessionID, 10*time.Second); err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	t.Log("✓ reply mirrored; session bound by its llmux-chosen id")
}

// ── TestSpawn_TrustDialogApproval ────────────────────────────────────────────
//
// In a folder Claude Code does not trust yet, the TUI first shows "Quick safety
// check ... ❯ No, exit". The relay must surface that to Discord as an approval
// (never auto-accept), and Approve must trust the folder (Down, Enter) so the
// spawn continues: the first prompt is delivered and the session is mirrored.
func TestSpawn_TrustDialogApproval(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19958", "19959"
	setupMock(t, mockPort, mockGWPort)

	const channel = "test-spawn-trust"
	spawnRelay(t, projects, stateDir, mockPort, channel, "user-allowed-001", "FAKE_CLAUDE_UNTRUSTED=1")

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-trust-1", channel, "user-allowed-001", false, "trust round trip prompt", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	approveID := waitForComponentID(t, mockPort, "Trust this folder", 30*time.Second)

	// Never auto-accepted: the dialog stays up and nothing is injected until approved.
	time.Sleep(1500 * time.Millisecond)
	var dialogPane string
	for _, s := range listLlmSessions() {
		if p := capturePane(s + ":0.0"); strings.Contains(p, "Quick safety check") {
			dialogPane = s + ":0.0"
		}
	}
	if dialogPane == "" {
		t.Fatal("the trust dialog was dismissed without anyone approving it")
	}
	if ids := transcriptSessionIDs(projects); len(ids) != 0 {
		t.Fatalf("a prompt was delivered before the folder was trusted: %v", ids)
	}

	if err := dispatchEvent(mockPort, interactionEvent("interaction-trust-001", channel, approveID, 2, nil)); err != nil {
		t.Fatal(err)
	}

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 40*time.Second); err != nil {
		t.Fatalf("no thread after the folder was trusted: %v\npane:\n%s", err, capturePane(dialogPane))
	}
	sessionID := waitForNewTranscript(t, projects, nil, 30*time.Second)
	if err := waitForPrompt(t, projects, sessionID, "trust round trip prompt", 30*time.Second); err != nil {
		t.Fatalf("first prompt not delivered after trust: %v", err)
	}
	if err := waitForMessageContent(t, mockPort, "Folder trusted", 10*time.Second); err != nil {
		t.Errorf("the approval message was not updated: %v", err)
	}
	if strings.Contains(capturePane(dialogPane), "Quick safety check") {
		t.Error("the trust dialog is still on screen after approval")
	}
	t.Log("✓ trust dialog surfaced to Discord; Approve trusted the folder and the spawn continued")
}

// ── TestSpawn_TrustDialogDenied ──────────────────────────────────────────────
//
// Deny (Escape) must never trust the folder: the spawn fails closed, nothing is
// injected, no thread is created, and the spawned session is torn down.
func TestSpawn_TrustDialogDenied(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19960", "19961"
	setupMock(t, mockPort, mockGWPort)

	const channel = "test-spawn-trustdeny"
	spawnRelay(t, projects, stateDir, mockPort, channel, "user-allowed-001", "FAKE_CLAUDE_UNTRUSTED=1")

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-trust-2", channel, "user-allowed-001", false, "must never be delivered", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	denyID := waitForComponentIDAt(t, mockPort, "Trust this folder", 1, 30*time.Second)
	if err := dispatchEvent(mockPort, interactionEvent("interaction-trust-002", channel, denyID, 2, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitForMessageContent(t, mockPort, "Trust denied", 10*time.Second); err != nil {
		t.Fatalf("denial not recorded: %v", err)
	}

	time.Sleep(3 * time.Second)
	if n := countThreads(mockPort); n != 0 {
		t.Fatalf("a thread was created for a denied folder")
	}
	if ids := transcriptSessionIDs(projects); len(ids) != 0 {
		t.Fatalf("a prompt reached a session whose folder was never trusted: %v", ids)
	}
	for _, s := range listLlmSessions() {
		if p := capturePane(s + ":0.0"); strings.Contains(p, "must never be delivered") {
			t.Fatalf("the prompt was typed into the untrusted session %s", s)
		}
	}
	t.Log("✓ trust denied: no prompt, no thread, nothing trusted")
}

// ── TestSpawn_FailureTeardownSparesLiveSession ───────────────────────────────
//
// A spawn that fails to bind must tear down only its own tmux session. A live,
// bound session (its pane and its sessions.json record) must survive, and the
// failure is announced exactly once.
func TestSpawn_FailureTeardownSparesLiveSession(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "llmux")
	killLlmSessionsCleanup(t)
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19942", "19943"
	setupMock(t, mockPort, mockGWPort)

	// Live, bound session: a running fake CLI with a transcript, recorded in
	// sessions.json with a thread.
	const liveName = "llm-live-spare-001"
	liveID := testSID(53)
	livePane := startFakePane(t, liveName, projects, nil, "--session-id", liveID)
	waitForPaneReady(t, livePane, 10*time.Second)
	submitInPane(t, livePane, "live session prompt")
	if err := waitForPrompt(t, projects, liveID, "live session prompt", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	writeSessionsJSON(t, stateDir, liveID, livePane, "thread-live-001", "test-spawn-tdown")

	// The spawned CLI looks ready and accepts input but never writes a transcript.
	stub := filepath.Join(t.TempDir(), "stub-never-writes")
	stubBody := "#!/bin/sh\nprintf '\\n────────────────────\\n❯ \\n────────────────────\\n  ? for shortcuts\\n'\ncat >/dev/null\n"
	if err := os.WriteFile(stub, []byte(stubBody), 0755); err != nil {
		t.Fatal(err)
	}
	startRelay(t, mustBin(t, "relay"), projects, stateDir, mockPort,
		"DISCORD_CHANNEL_ID=test-spawn-tdown", "DISCORD_USER_ID=user-tdown-001",
		"RELAY_AGENT_CMD="+stub, "RELAY_SPAWN_BIND_TIMEOUT_MS=3000")

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-tdown-1", "test-spawn-tdown", "user-tdown-001", false, "teardown: this spawn never binds", nil)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// The failed spawn appears, then is torn down.
	var spawnName string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && spawnName == "" {
		for _, s := range listLlmSessions() {
			if s != liveName {
				spawnName = s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if spawnName == "" {
		t.Fatal("no spawned llm- session ever appeared")
	}
	t.Logf("failed spawn session: %s", spawnName)

	// Exactly one failure notice (wait for it, then let any duplicate settle).
	const notice = "Could not start a new Claude Code session"
	if err := waitForMessageContent(t, mockPort, notice, 40*time.Second); err != nil {
		t.Fatalf("no failure notice posted: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	msgs, _ := getSentMessages(mockPort)
	notices := 0
	for _, m := range msgs {
		if strings.Contains(contentOf(m), notice) {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("expected exactly one failure notice, got %d", notices)
	}

	if err := exec.Command("tmux", "has-session", "-t", "="+spawnName).Run(); err == nil {
		t.Fatalf("failed spawn session %s still running after teardown", spawnName)
	}
	if err := exec.Command("tmux", "has-session", "-t", "="+liveName).Run(); err != nil {
		t.Fatalf("live session %s was killed by the failed spawn's teardown", liveName)
	}
	if !strings.Contains(capturePane(livePane), "❯") {
		t.Fatalf("live pane damaged: %q", capturePane(livePane))
	}
	if err := waitForSessionRecord(stateDir, liveID, 2*time.Second); err != nil {
		t.Fatalf("live session record removed from sessions.json: %v", err)
	}
	if n := countThreads(mockPort); n != 0 {
		t.Fatalf("a failed spawn created %d thread(s)", n)
	}
	t.Log("✓ live pane + record survive; failed spawn pane gone; one notice; no thread")
}
