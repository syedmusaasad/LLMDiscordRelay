//go:build integration

// mirror_test.go: integration tests for milestone 1 (mirror only): transcripts
// that already exist when the relay attaches are replayed into a Discord thread.
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historicalSession seeds a transcript for sessionID and a resume marker that
// points at a dead pane, the way a session from an earlier relay run looks.
func historicalSession(t *testing.T, projects, stateDir, name string, sessionID string) *txWriter {
	t.Helper()
	writeMarker(t, stateDir, name, name+":0.0", sessionID, true)
	return newTx(t, projects, sessionID)
}

// ── TestInvariant2_FirstSightReplay ───────────────────────────────────────────
//
// Invariant 2: a completed first exchange already on disk at attach still
// mirrors. fake-claude completes a full turn before the relay starts; the relay
// then attaches and the thread must show the turn.
func TestInvariant2_FirstSightReplay(t *testing.T) {
	mustBin(t, "tmux")
	fake := mustBin(t, "fake-claude")
	relayBin := mustBin(t, "relay")

	projects := t.TempDir()
	stateDir := t.TempDir()
	sessionID := testSID(10)

	script := []map[string]string{{"prompt": "ping", "reply": "Pong!"}}
	scriptData, _ := json.Marshal(script)
	scriptFile := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptFile, scriptData, 0644); err != nil {
		t.Fatal(err)
	}

	mockPort, mockGWPort := "19876", "19877"
	setupMock(t, mockPort, mockGWPort)

	// ── Step 1: run fake-claude to completion BEFORE the relay starts ───────
	fakeCmd := exec.Command(fake, "--session-id", sessionID)
	fakeCmd.Env = append(os.Environ(),
		"RELAY_CLAUDE_PROJECTS="+projects,
		"FAKE_CLAUDE_SCRIPT="+scriptFile,
	)
	// Pipe "ping\n" then close stdin: the script turn runs, EOF ends the process.
	fakeCmd.Stdin = strings.NewReader("ping\n")
	fakeCmd.Stdout = io.Discard
	fakeCmd.Stderr = io.Discard
	if err := fakeCmd.Run(); err != nil {
		t.Logf("fake-claude exited: %v", err)
	}
	if err := waitForPrompt(t, projects, sessionID, "ping", 3*time.Second); err != nil {
		t.Fatalf("session did not complete before the relay started: %v", err)
	}
	if !strings.Contains(transcriptRaw(projects, sessionID), "turn_duration") {
		t.Fatalf("turn did not finish: %s", transcriptRaw(projects, sessionID))
	}

	// ── Step 2: the marker llmux would have written ─────────────────────────
	writeMarker(t, stateDir, "llm-test-firstsight", "llm-test-firstsight:0.0", sessionID, true)

	// ── Step 3: start the relay ─────────────────────────────────────────────
	startRelay(t, relayBin, projects, stateDir, mockPort, "DISCORD_USER_ID=user-allowed-001")

	// ── Step 4: the thread shows the completed turn ─────────────────────────
	if err := waitForSentMessages(t, mockPort, 3, 15*time.Second); err != nil {
		t.Fatalf("relay did not send expected messages: %v", err)
	}
	if err := waitForMessageContent(t, mockPort, "Pong!", 10*time.Second); err != nil {
		t.Fatalf("expected 'Pong!' in mirrored messages: %v", err)
	}
	msgs, _ := getSentMessages(mockPort)
	t.Logf("✓ first-sight replay: turn from completed session visible in thread (%d messages)", len(msgs))
}

// ── TestMirror_EmbedFlip ──────────────────────────────────────────────────────
//
// A tool call that completes has its embed flipped from "running" to "done".
func TestMirror_EmbedFlip(t *testing.T) {
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19878", "19879"
	setupMock(t, mockPort, mockGWPort)

	historicalSession(t, projects, stateDir, "llm-test-embedflip", testSID(11)).
		User("list the files").
		ToolUse("toolu_flip001", "Bash", map[string]any{"command": "ls"}).
		ToolResult("toolu_flip001", "file1.txt", false).
		Text("Done listing files.").
		TurnEnd()

	startRelay(t, relayBin, projects, stateDir, mockPort)

	if err := waitForMessageContent(t, mockPort, "✅ done", 15*time.Second); err != nil {
		t.Fatalf("embed flip (done) not found: %v", err)
	}
	t.Log("✓ tool embed flipped to ✅ done")
}

// ── TestMirror_NoOutputLine ───────────────────────────────────────────────────
//
// When a turn ends with no assistant text, a "turn finished without output"
// line appears.
func TestMirror_NoOutputLine(t *testing.T) {
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19880", "19881"
	setupMock(t, mockPort, mockGWPort)

	historicalSession(t, projects, stateDir, "llm-test-noout", testSID(12)).
		User("say nothing").
		TurnEnd()

	startRelay(t, relayBin, projects, stateDir, mockPort)

	if err := waitForMessageContent(t, mockPort, "Turn finished without output", 15*time.Second); err != nil {
		t.Fatalf("no-output line not found: %v", err)
	}
	t.Log("✓ 'Turn finished without output' line posted")
}

// ── TestMirror_InterruptedTurn ────────────────────────────────────────────────
//
// An interrupt (Esc, or "No" on a permission prompt) ends the turn without a
// turn_duration line; the thread says so instead of "finished without output".
func TestMirror_InterruptedTurn(t *testing.T) {
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19888", "19889"
	setupMock(t, mockPort, mockGWPort)

	historicalSession(t, projects, stateDir, "llm-test-interrupt", testSID(13)).
		User("run something risky").
		ToolUse("toolu_int001", "Bash", map[string]any{"command": "rm -rf /"}).
		ToolResult("toolu_int001", "The user doesn't want to proceed with this tool use.", true).
		Interrupt()

	startRelay(t, relayBin, projects, stateDir, mockPort)

	if err := waitForMessageContent(t, mockPort, "Turn interrupted", 15*time.Second); err != nil {
		t.Fatalf("interrupt line not found: %v", err)
	}
	msgs, _ := getSentMessages(mockPort)
	for _, m := range msgs {
		if strings.Contains(contentOf(m), "finished without output") {
			t.Fatalf("an interrupted turn was reported as 'finished without output'")
		}
	}
	t.Log("✓ interrupted turn reported as interrupted")
}

// ── TestMirror_ToolErrorSoftening ────────────────────────────────────────────
//
// A tool error is posted as a failed embed.
func TestMirror_ToolErrorSoftening(t *testing.T) {
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19882", "19883"
	setupMock(t, mockPort, mockGWPort)

	historicalSession(t, projects, stateDir, "llm-test-toolerr", testSID(14)).
		User("run a bad command").
		ToolUse("toolu_err001", "Bash", map[string]any{"command": "nosuchcmd"}).
		ToolResult("toolu_err001", "command not found", true).
		Text("I encountered an error but handled it.").
		TurnEnd()

	startRelay(t, relayBin, projects, stateDir, mockPort)

	if err := waitForMessageContent(t, mockPort, "❌ failed", 15*time.Second); err != nil {
		t.Fatalf("tool error embed not found: %v", err)
	}
	t.Log("✓ tool error embed posted with ❌ failed")
}

// ── TestMirror_LongTextChunksToFollowups ──────────────────────────────────────
//
// A 5000-char assistant reply must be split into >=3 Discord messages whose
// concatenation equals the original reply.
func TestMirror_LongTextChunksToFollowups(t *testing.T) {
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19884", "19885"
	setupMock(t, mockPort, mockGWPort)

	// Paragraph breaks let ChunkText split; distinct words avoid dedup issues.
	makeParaWith := func(word string, n int) string {
		s := ""
		for i := 0; i < n; i++ {
			s += word + " "
		}
		return s
	}
	longReply := makeParaWith("alpha", 200) + "\n\n" + makeParaWith("bravo", 200) + "\n\n" + makeParaWith("charlie", 200)
	if len(longReply) < 4001 {
		t.Fatalf("test setup: longReply too short (%d chars), expected >4000", len(longReply))
	}

	historicalSession(t, projects, stateDir, "llm-test-longtext", testSID(15)).
		User("write a long answer").
		Text(longReply).
		TurnEnd()

	startRelay(t, relayBin, projects, stateDir, mockPort)

	// The session-ended marker means the session was fully mirrored.
	if err := waitForMessageContent(t, mockPort, "⏹ Session ended", 20*time.Second); err != nil {
		t.Fatalf("session end not seen: %v", err)
	}

	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}
	seen := make(map[string]bool)
	var textChunks []string
	for _, m := range msgs {
		c := contentOf(m)
		if c == "" || seen[c] {
			continue
		}
		if len(c) > 50 && strings.Contains(longReply, c) {
			seen[c] = true
			textChunks = append(textChunks, c)
		}
	}
	if len(textChunks) < 3 {
		t.Errorf("expected >=3 text chunk messages in thread, got %d; chunks: %v", len(textChunks), textChunks)
	}
	if concatenated := strings.Join(textChunks, ""); concatenated != longReply {
		t.Errorf("chunks do not reassemble to original reply:\noriginal len=%d\nconcatenated len=%d", len(longReply), len(concatenated))
	}
	t.Logf("✓ long text split into %d chunks, concatenation matches (len=%d)", len(textChunks), len(longReply))
}

// ── TestMirror_ToolErrorSoftenedOnCleanEnd ────────────────────────────────────
//
// A tool error followed by a clean turn end: the error embed is softened
// in place (⚠️ warning, "turn completed").
func TestMirror_ToolErrorSoftenedOnCleanEnd(t *testing.T) {
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19886", "19887"
	setupMock(t, mockPort, mockGWPort)

	historicalSession(t, projects, stateDir, "llm-test-toolsoft", testSID(16)).
		User("run a failing command").
		ToolUse("toolu_soft001", "Bash", map[string]any{"command": "false"}).
		ToolResult("toolu_soft001", "failed", true).
		Text("Handled the error gracefully.").
		TurnEnd()

	startRelay(t, relayBin, projects, stateDir, mockPort)

	if err := waitForMessageContent(t, mockPort, "❌ failed", 15*time.Second); err != nil {
		t.Fatalf("tool error embed not found: %v", err)
	}
	t.Log("✓ tool error embed posted with ❌ failed")
	if err := waitForMessageContent(t, mockPort, "turn completed", 15*time.Second); err != nil {
		t.Fatalf("softened tool embed not found (expected 'turn completed'): %v", err)
	}
	t.Log("✓ tool error softened to ⚠️ warning with 'turn completed' after a clean turn end")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func startMockDiscord(t *testing.T, bin, port, gwPort string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("MOCK_DISCORD_PORT=%s", port),
		fmt.Sprintf("MOCK_DISCORD_GATEWAY_PORT=%s", gwPort),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mock-discord: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd
}

// setupMock starts mock-discord on the given ports and waits until it answers.
func setupMock(t *testing.T, mockPort, gwPort string) {
	t.Helper()
	startMockDiscord(t, mustBin(t, "mock-discord"), mockPort, gwPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}
}

// startRelay starts the relay against mock-discord with the given transcript
// root and state dir. extra KEY=VALUE pairs come last and override the defaults.
func startRelay(t *testing.T, bin, projects, stateDir, mockPort string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-channel-001",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_CLAUDE_PROJECTS=%s", projects),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-allowed-001",
	)
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("HTTP %s not ready after %s", url, timeout)
}

func waitForSentMessages(t *testing.T, mockPort string, minCount int, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := getSentMessages(mockPort)
		if err == nil && len(msgs) >= minCount {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	msgs, _ := getSentMessages(mockPort)
	return fmt.Errorf("wanted %d messages, got %d after %s", minCount, len(msgs), timeout)
}

func waitForMessageContent(t *testing.T, mockPort, substr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := getSentMessages(mockPort)
		if err == nil {
			for _, m := range msgs {
				if strings.Contains(contentOf(m), substr) {
					return nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	msgs, _ := getSentMessages(mockPort)
	contents := make([]string, 0, len(msgs))
	for _, m := range msgs {
		contents = append(contents, contentOf(m))
	}
	return fmt.Errorf("content %q not found after %s; messages: %v", substr, timeout, contents)
}

func getSentMessages(mockPort string) ([]json.RawMessage, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/control/sent", mockPort))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var msgs []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func contentOf(msg json.RawMessage) string {
	var m struct {
		Content string `json:"content"`
	}
	json.Unmarshal(msg, &m)
	return m.Content
}
