//go:build integration

// inject_test.go: integration tests for milestone 2 (inject thread replies into
// the pane) and for the pane-driven approval / question round-trips.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
)

// seedHistory writes a small finished exchange so the relay has something to
// replay (and therefore creates a thread) as soon as it pairs the session.
func seedHistory(t *testing.T, projects, sessionID string) {
	t.Helper()
	newTx(t, projects, sessionID).User("earlier question").Text("I am ready").TurnEnd()
}

// interactionEvent builds an INTERACTION_CREATE gateway event (a button click
// or a select-menu choice) from the allowed user.
func interactionEvent(id, channelID, customID string, componentType int, values []string) map[string]interface{} {
	data := map[string]interface{}{"custom_id": customID, "component_type": componentType}
	if values != nil {
		data["values"] = values
	}
	return map[string]interface{}{"t": "INTERACTION_CREATE", "d": map[string]interface{}{
		"id": id, "application_id": "app", "type": 3, "token": "token", "version": 1, "channel_id": channelID,
		"member": map[string]interface{}{"user": map[string]interface{}{"id": "user-allowed-001", "bot": false}},
		"data":   data,
	}}
}

// ── TestInject_ReadinessProbe ─────────────────────────────────────────────────
//
// The readiness probe waits for the idle input line before sending keys, and
// the prompt lands exactly once in the transcript.
//
// fake-claude starts with FAKE_CLAUDE_SWALLOW_UNTIL_READY=1 and a 3 s ready
// delay, so any keys sent before the idle input line is drawn are discarded.
func TestInject_ReadinessProbe(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19884", "19885"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(20)
	seedHistory(t, projects, sessionID)

	sessionName := "llm-inject-rp"
	pane := startFakePane(t, sessionName, projects,
		[]string{"FAKE_CLAUDE_SWALLOW_UNTIL_READY=1", "FAKE_CLAUDE_READY_DELAY_MS=3000"}, "--resume", sessionID)
	writeMarker(t, stateDir, sessionName, pane, sessionID, true)

	startRelay(t, relayBin, projects, stateDir, mockPort)

	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 10*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	// A MESSAGE_CREATE delivered during the gateway's READY handshake would be
	// lost and look like a readiness-probe failure: wait for the session record.
	if err := waitForSessionRecord(stateDir, sessionID, 5*time.Second); err != nil {
		t.Fatalf("relay did not register claimed session before dispatch: %v", err)
	}

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-dispatch-001", threadID, "user-allowed-001", false, "hello from discord", nil)); err != nil {
		t.Fatalf("dispatch event: %v", err)
	}

	if err := waitForPrompt(t, projects, sessionID, "hello from discord", 20*time.Second); err != nil {
		t.Fatalf("injected prompt not found in transcript: %v", err)
	}
	t.Log("✓ readiness probe: prompt landed in the transcript")

	time.Sleep(1 * time.Second)
	if n := countPrompts(projects, sessionID, "hello from discord"); n != 1 {
		t.Errorf("expected prompt to appear exactly once; got %d occurrences", n)
	}
	t.Log("✓ prompt landed exactly once")
}

// ── TestInject_MultiLineArrivesAsOnePrompt ───────────────────────────────────
//
// A 3-line Discord message must reach Claude Code as ONE prompt: send-keys -l
// would submit at the first newline, so the relay pastes it (bracketed paste)
// and presses Enter once.
func TestInject_MultiLineArrivesAsOnePrompt(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19950", "19951"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(21)
	seedHistory(t, projects, sessionID)
	sessionName := "llm-inject-multiline"
	pane := startFakePane(t, sessionName, projects, nil, "--resume", sessionID)
	waitForPaneReady(t, pane, 10*time.Second)
	writeMarker(t, stateDir, sessionName, pane, sessionID, true)

	startRelay(t, relayBin, projects, stateDir, mockPort)
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 10*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	if err := waitForSessionRecord(stateDir, sessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	const text = "first line of the request\nsecond line with details\nthird line: please answer"
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-multiline-001", threadID, "user-allowed-001", false, text, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitForPrompt(t, projects, sessionID, "third line: please answer", 20*time.Second); err != nil {
		t.Fatalf("multi-line message never arrived as a prompt: %v\npane:\n%s", err, capturePane(pane))
	}
	time.Sleep(1 * time.Second)

	var got []string
	for _, p := range transcriptPrompts(projects, sessionID) {
		if p != "earlier question" {
			got = append(got, p)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the 3-line message was split into %d prompts: %q", len(got), got)
	}
	if got[0] != text {
		t.Fatalf("prompt text differs from the Discord message:\n got: %q\nwant: %q", got[0], text)
	}
	t.Log("✓ 3-line Discord message arrived as exactly one 3-line prompt")
}

// ── TestInject_ForeignAuthorDropped ─────────────────────────────────────────
//
// Messages from authors other than DISCORD_USER_ID (and from bots) are ignored.
func TestInject_ForeignAuthorDropped(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19886", "19887"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(22)
	// No live pane: inject must never fire.
	writeMarker(t, stateDir, "llm-inject-fa", "nonexistent-pane:0.0", sessionID, true)
	newTx(t, projects, sessionID).User("status?").Text("I am ready").TurnEnd()

	startRelay(t, relayBin, projects, stateDir, mockPort)
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 10*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	time.Sleep(500 * time.Millisecond)
	clearSentMessages(mockPort)

	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-bot-001", threadID, "bot-user-999", true, "I am a bot", nil)); err != nil {
		t.Fatalf("dispatch bot event: %v", err)
	}
	if err := dispatchEvent(mockPort, buildMessageEvent("msg-foreign-001", threadID, "user-foreign-999", false, "I am an outsider", nil)); err != nil {
		t.Fatalf("dispatch foreign event: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	raw := transcriptRaw(projects, sessionID)
	if strings.Contains(raw, "I am a bot") || strings.Contains(raw, "I am an outsider") {
		t.Errorf("foreign author text reached the session:\n%s", raw)
	}
	if ids := transcriptSessionIDs(projects); len(ids) != 1 {
		t.Errorf("a foreign message caused a new session: %v", ids)
	}
	if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
		for _, s := range strings.Fields(string(out)) {
			if strings.HasPrefix(s, "llm-") && s != "llm-inject-fa" {
				t.Errorf("a foreign message spawned tmux session %s", s)
			}
		}
	}
	t.Log("✓ foreign author dropped: nothing injected, nothing spawned")
}

// ── TestInject_AttachmentPath ────────────────────────────────────────────────
//
// Image attachments are downloaded to the staging directory with sanitised
// filenames and the local path is correctly computed.
func TestInject_AttachmentPath(t *testing.T) {
	stateDir := t.TempDir()

	attachContent := []byte("fake-image-data")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(attachContent)
	}))
	defer ts.Close()

	inj := inject.New(stateDir)
	sessionID := testSID(23)

	atts := []inject.Attachment{
		{Filename: "my image\t.png", URL: ts.URL + "/image.png"},
	}

	paths, err := inj.DownloadAttachments(sessionID, atts)
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read downloaded file %s: %v", paths[0], err)
	}
	if string(data) != string(attachContent) {
		t.Errorf("downloaded content mismatch: got %q, want %q", data, attachContent)
	}
	if base := filepath.Base(paths[0]); strings.Contains(base, "\t") {
		t.Errorf("filename contains tab character: %q", base)
	}
	expectedDir := filepath.Join(stateDir, "staging", sessionID)
	if !strings.HasPrefix(paths[0], expectedDir) {
		t.Errorf("path %q should be under %q", paths[0], expectedDir)
	}
	t.Logf("✓ attachment downloaded to %s (sanitised from %q)", paths[0], atts[0].Filename)
}

// startPromptPane starts a fake-claude pane whose first turn blocks on a tool
// prompt (set through env), submits a prompt so the transcript exists, and
// returns the pane and session id. The caller then starts the relay.
func startPromptPane(t *testing.T, projects, stateDir, name string, sessionID string, env ...string) string {
	t.Helper()
	pane := startFakePane(t, name, projects, env, "--session-id", sessionID)
	waitForPaneReady(t, pane, 10*time.Second)
	submitInPane(t, pane, "please list the files")
	if err := waitForPrompt(t, projects, sessionID, "please list the files", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, stateDir, name, pane, sessionID, false)
	return pane
}

// TestApproval_AutoDenyTimeout verifies a pane-only permission prompt creates
// buttons and, when unanswered, receives the Deny recipe (Escape) and is edited
// to record the automatic denial.
func TestApproval_AutoDenyTimeout(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19920", "19921"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(30)
	pane := startPromptPane(t, projects, stateDir, "llm-approval-timeout", sessionID, "FAKE_CLAUDE_PERMISSION_PROMPT=1")
	if err := waitForPaneText(t, pane, "Do you want to proceed?", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	startRelay(t, relayBin, projects, stateDir, mockPort, "APPROVAL_TIMEOUT_SECONDS=1")

	if err := waitForMessageContent(t, mockPort, "Permission denied automatically", 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, pane, "Interrupted", 5*time.Second); err != nil {
		t.Fatalf("deny recipe did not resolve the pane: %v", err)
	}
	if !strings.Contains(transcriptRaw(projects, sessionID), "[Request interrupted by user for tool use]") {
		t.Fatal("the automatic denial did not interrupt the turn")
	}
}

func TestInteraction_ApproveDrivesApproveRecipe(t *testing.T) {
	testInteractionApproval(t, "approve")
}

func TestInteraction_DenyDrivesDenyRecipe(t *testing.T) {
	testInteractionApproval(t, "deny")
}

// testInteractionApproval proves each approval action is acknowledged and
// drives its own distinct key recipe in the fake pane: Approve sends "1" (the
// tool runs and the turn completes), Deny sends Escape (the turn is interrupted).
func testInteractionApproval(t *testing.T, action string) {
	t.Helper()
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19924", "19925"
	if action == "deny" {
		mockPort, mockGWPort = "19952", "19953"
	}
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(31)
	if action == "deny" {
		sessionID = testSID(32)
	}
	pane := startPromptPane(t, projects, stateDir, "llm-interaction-"+action, sessionID, "FAKE_CLAUDE_PERMISSION_PROMPT=1")
	if err := waitForPaneText(t, pane, "Do you want to proceed?", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	startRelay(t, relayBin, projects, stateDir, mockPort)
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	componentID := waitForComponentID(t, mockPort, "Permission required", 10*time.Second)
	if action == "deny" {
		componentID = waitForComponentIDAt(t, mockPort, "Permission required", 1, 10*time.Second)
	}
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, interactionEvent("interaction-"+action+"-001", threadID, componentID, 2, nil)); err != nil {
		t.Fatal(err)
	}

	if action == "approve" {
		if err := waitForPaneText(t, pane, "(ack turn 1)", 10*time.Second); err != nil {
			t.Fatalf("approve recipe did not let the tool run: %v", err)
		}
		if strings.Contains(capturePane(pane), "Interrupted") {
			t.Fatalf("approve also interrupted the turn:\n%s", capturePane(pane))
		}
		if strings.Contains(transcriptRaw(projects, sessionID), "[Request interrupted") {
			t.Fatal("approve interrupted the turn")
		}
	} else {
		if err := waitForPaneText(t, pane, "Interrupted", 10*time.Second); err != nil {
			t.Fatalf("deny recipe did not reach the pane: %v", err)
		}
		if strings.Contains(capturePane(pane), "(ack turn 1)") {
			t.Fatalf("deny also let the tool run:\n%s", capturePane(pane))
		}
	}
	if !sentContainsCallback(mockPort) {
		t.Fatal("interaction acknowledgement callback was not sent")
	}
}

// TestApproval_SecondPromptGetsButtons verifies prompt state resets after the
// first permission is finalized, allowing the second prompt to post buttons.
func TestApproval_SecondPromptGetsButtons(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19926", "19927"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(33)
	pane := startPromptPane(t, projects, stateDir, "llm-second-approval", sessionID, "FAKE_CLAUDE_PERMISSION_PROMPTS=2")
	if err := waitForPaneText(t, pane, "Do you want to proceed?", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	startRelay(t, relayBin, projects, stateDir, mockPort)
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	first := waitForComponentID(t, mockPort, "Permission required", 10*time.Second)
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, interactionEvent("second-first", threadID, first, 2, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, pane, "ls # 2", 10*time.Second); err != nil {
		t.Fatalf("second prompt never appeared: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	count := 0
	for time.Now().Before(deadline) {
		count = componentMessageCount(mockPort, "Permission required")
		if count >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if count != 2 {
		t.Fatalf("got %d approval button messages, want 2; pane:\n%s", count, capturePane(pane))
	}
}

// TestQuestionPicker_MenuSelection verifies the AskUserQuestion contract: the
// pane's real options (not "Type something." / "Chat about this") become a
// Discord select menu and a selected value drives its Down+Enter recipe.
func TestQuestionPicker_MenuSelection(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19922", "19923"
	setupMock(t, mockPort, mockGWPort)

	sessionID := testSID(34)
	pane := startPromptPane(t, projects, stateDir, "llm-question-picker", sessionID, "FAKE_CLAUDE_QUESTION_PICKER=1")
	if err := waitForPaneText(t, pane, "Which color do you prefer?", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	startRelay(t, relayBin, projects, stateDir, mockPort)
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 15*time.Second); err != nil {
		t.Fatal(err)
	}

	var questionID string
	var options []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && questionID == "" {
		messages, _ := getSentMessages(mockPort)
		for _, message := range messages {
			var body struct {
				Content    string `json:"content"`
				Components []struct {
					Components []struct {
						CustomID string `json:"custom_id"`
						Options  []struct {
							Label string `json:"label"`
						} `json:"options"`
					} `json:"components"`
				} `json:"components"`
			}
			if json.Unmarshal(message, &body) == nil && strings.Contains(body.Content, "Which color do you prefer?") &&
				len(body.Components) > 0 && len(body.Components[0].Components) > 0 {
				questionID = body.Components[0].Components[0].CustomID
				for _, o := range body.Components[0].Components[0].Options {
					options = append(options, o.Label)
				}
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if questionID == "" {
		t.Fatal("question select menu was not posted")
	}
	if len(options) != 0 && strings.Join(options, ",") != "Red,Green" {
		t.Fatalf("select menu options = %v, want only the real options Red,Green", options)
	}

	waitForRelayReady(t, stateDir)
	// Value "1" is the second option: Down once, then Enter.
	if err := dispatchEvent(mockPort, interactionEvent("interaction-question-001", threadID, questionID, 3, []string{"1"})); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, pane, "answered: Green", 10*time.Second); err != nil {
		t.Fatalf("select recipe did not choose the second option: %v", err)
	}
	if !strings.Contains(transcriptRaw(projects, sessionID), "User has answered your questions: Green") {
		t.Fatal("the answer never reached the transcript")
	}
}

// ── helpers specific to inject tests ─────────────────────────────────────────

// buildMessageEvent constructs a MESSAGE_CREATE gateway event map.
func buildMessageEvent(id, channelID, authorID string, isBot bool, content string, attachments []map[string]interface{}) map[string]interface{} {
	atts := []interface{}{}
	for _, a := range attachments {
		atts = append(atts, a)
	}
	return map[string]interface{}{
		"t": "MESSAGE_CREATE",
		"d": map[string]interface{}{
			"id":         id,
			"channel_id": channelID,
			"content":    content,
			"author": map[string]interface{}{
				"id":  authorID,
				"bot": isBot,
			},
			"attachments": atts,
		},
	}
}

// dispatchEvent sends a gateway event to mock-discord's /control/dispatch.
func dispatchEvent(mockPort string, ev interface{}) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%s/control/dispatch", mockPort),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dispatch: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// clearSentMessages sends DELETE /control/sent to reset the sent log.
func clearSentMessages(mockPort string) {
	req, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("http://127.0.0.1:%s/control/sent", mockPort), nil)
	http.DefaultClient.Do(req) //nolint:errcheck
}

// waitForThreadCreation polls /control/sent until a CreateThread request body
// (type:11) is found. It sets threadID to the mock-assigned ID (second sequential ID).
func waitForThreadCreation(t *testing.T, mockPort string, threadID *string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := getSentMessages(mockPort)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// Look for a CreateThread request body: {"name":"ses_...","type":11,...}
		for i, m := range msgs {
			var body struct {
				Name string `json:"name"`
				Type int    `json:"type"`
			}
			if err := json.Unmarshal(m, &body); err != nil {
				continue
			}
			if body.Type == 11 {
				// The relay made requests in order: (1) create root message, (2) create thread.
				// mock-discord assigns sequential IDs: root=1, thread=2, ...
				// The thread ID = i+2 (1-indexed, with root being msg #1).
				// Actually: each POST to /messages or /threads gets a unique ID.
				// Root message is request index 0 → ID "1".
				// Thread creation is request index i → ID = fmt.Sprint(i+1).
				*threadID = fmt.Sprintf("%d", i+1)
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("thread creation not found in sent messages after %s", timeout)
}

func waitForSessionRecord(stateDir, sessionID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	path := filepath.Join(stateDir, "sessions.json")
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var records []struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(data, &records) == nil {
				for _, record := range records {
					if record.SessionID == sessionID {
						return nil
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("session %s not present in %s after %s", sessionID, path, timeout)
}

// waitForRelayReady blocks until the relay has written its gateway-ready file
// (written on the gateway READY event). Every test that dispatches gateway
// events must call it first; it is the single shared readiness wait.
func waitForRelayReady(t *testing.T, stateDir string) {
	t.Helper()
	path := filepath.Join(stateDir, "gateway-ready")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("relay gateway not ready: %s absent after 15s", path)
}

func waitForComponentID(t *testing.T, port, content string, timeout time.Duration) string {
	return waitForComponentIDAt(t, port, content, 0, timeout)
}

func waitForComponentIDAt(t *testing.T, port, content string, index int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := getSentMessages(port)
		for _, message := range msgs {
			var body struct {
				Content    string `json:"content"`
				Components []struct {
					Components []struct {
						CustomID string `json:"custom_id"`
					} `json:"components"`
				} `json:"components"`
			}
			if json.Unmarshal(message, &body) == nil && strings.Contains(body.Content, content) && len(body.Components) > 0 && len(body.Components[0].Components) > index {
				return body.Components[0].Components[index].CustomID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("component message %q not posted", content)
	return ""
}
func componentMessageCount(port, content string) int {
	msgs, _ := getSentMessages(port)
	count := 0
	for _, message := range msgs {
		var body struct {
			Content    string            `json:"content"`
			Components []json.RawMessage `json:"components"`
		}
		if json.Unmarshal(message, &body) == nil && strings.Contains(body.Content, content) && len(body.Components) > 0 {
			count++
		}
	}
	return count
}
func sentContainsCallback(port string) bool {
	msgs, _ := getSentMessages(port)
	for _, message := range msgs {
		var body struct {
			Type int `json:"type"`
		}
		if json.Unmarshal(message, &body) == nil && body.Type == 6 {
			return true
		}
	}
	return false
}

// TestGateway_DispatchBeforeReadyIsDelivered dispatches a gateway event
// immediately after the relay process starts, while its gateway handshake is
// provably incomplete: mock-discord delays READY by MOCK_READY_DELAY_MS, and the
// test fails hard if the relay's gateway-ready file already exists when the
// dispatch is issued. mock-discord must hold the event until a READY session
// is consuming, and the relay must then handle it exactly once.
func TestGateway_DispatchBeforeReadyIsDelivered(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	projects, stateDir := t.TempDir(), t.TempDir()
	mockPort, mockGWPort := "19930", "19931"
	t.Setenv("MOCK_READY_DELAY_MS", "1500") // inherited by mock-discord only via startMockDiscord's os.Environ
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}
	sessionID := testSID(24)
	seedHistory(t, projects, sessionID)

	sessionName := "llm-gateway-dbr"
	pane := startFakePane(t, sessionName, projects, nil, "--resume", sessionID)
	waitForPaneReady(t, pane, 10*time.Second)

	// Pre-seed the claimed thread so the event has a handler target without
	// waiting on the relay: the only thing under test is gateway delivery.
	const threadID = "thread-before-ready-001"
	writeSessionsJSON(t, stateDir, sessionID, pane, threadID, "test-channel-001")

	startRelay(t, relayBin, projects, stateDir, mockPort)

	// Deliberately NO waitForRelayReady: dispatch races the relay's handshake.
	// mock-discord holds READY back for 1.5s after IDENTIFY, so the relay cannot
	// be ready when this dispatch is issued; assert that precondition instead of
	// assuming it. The dispatch itself is held by the mock until READY, so it
	// runs in a goroutine and its result is collected below.
	readyFile := filepath.Join(stateDir, "gateway-ready")
	ev := buildMessageEvent("msg-dbr-001", threadID, "user-allowed-001", false, "before ready hello", nil)
	dispatchErr := make(chan error, 1)
	go func() { dispatchErr <- dispatchEvent(mockPort, ev) }()
	if _, err := os.Stat(readyFile); err == nil {
		t.Fatalf("precondition violated: relay gateway already ready at dispatch time (%s exists)", readyFile)
	}
	select {
	case err := <-dispatchErr:
		t.Fatalf("dispatch returned before the gateway was READY (err=%v); mock did not hold it", err)
	case <-time.After(300 * time.Millisecond):
		// still held: no READY session yet
	}
	if _, err := os.Stat(readyFile); err == nil {
		t.Fatalf("precondition violated: relay became ready within 300ms of dispatch despite MOCK_READY_DELAY_MS")
	}
	select {
	case err := <-dispatchErr:
		if err != nil {
			t.Fatalf("dispatch before ready: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("held dispatch never released after READY")
	}

	if err := waitForPrompt(t, projects, sessionID, "before ready hello", 20*time.Second); err != nil {
		t.Fatalf("event dispatched before READY was not handled: %v", err)
	}
	// Exactly once: allow any erroneous duplicate delivery/injection to settle.
	time.Sleep(2 * time.Second)
	if n := countPrompts(projects, sessionID, "before ready hello"); n != 1 {
		t.Fatalf("expected event handled exactly once; got %d occurrences", n)
	}
	waitForRelayReady(t, stateDir)
}

// TestGateway_DispatchWithoutSessionFailsClearly: with no relay connected, the
// mock must not silently queue the event; it fails with an explicit error.
func TestGateway_DispatchWithoutSessionFailsClearly(t *testing.T) {
	mockDS := mustBin(t, "mock-discord")
	t.Setenv("MOCK_DISPATCH_READY_TIMEOUT_MS", "300")
	mockPort, mockGWPort := "19932", "19933"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}
	err := dispatchEvent(mockPort, buildMessageEvent("msg-nosession", "t", "u", false, "x", nil))
	if err == nil || !strings.Contains(err.Error(), "no gateway session sent IDENTIFY and received READY") {
		t.Fatalf("expected explicit no-READY-session error, got %v", err)
	}
}
