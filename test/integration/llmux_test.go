//go:build integration

// llmux_test.go: integration tests for cmd/llmux.
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

// TestLlmux_MarkerBeforeSpawn verifies that llmux writes the pending-<name> marker
// file BEFORE spawning the the agent process. This ensures the relay can pair the
// session even if the process starts and creates its transcript before llmux writes
// the marker.
//
// Approach: point RELAY_AGENT_CMD at a stub that records whether the marker
// existed at the child's own startup.
func TestLlmux_MarkerBeforeSpawn(t *testing.T) {
	mustBin(t, "tmux")
	llmuxBin := mustBin(t, "llmux")

	stateDir := llmuxStateDir(t)
	logFile := filepath.Join(t.TempDir(), "marker-check.log")

	// The pending marker is the durable pre-spawn contract. The child observes
	// it before doing anything else.
	stubScript := filepath.Join(t.TempDir(), "stub-agent")
	stubContent := fmt.Sprintf(`#!/bin/sh
# Stub agent: observe the marker at process startup, then stay alive.
marker_found=false
for marker in "$RELAY_STATE_DIR"/pending-*; do
  if [ -e "$marker" ]; then marker_found=true; fi
done
printf 'marker_found=%%s args=%%s %%s\n' "$marker_found" "$1" "$2" > "%s"
# Sleep briefly so tmux session stays alive long enough to be useful.
sleep 1
`, logFile)
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Run llmux with the stub as the agent command.
	cmd := exec.Command(llmuxBin)
	var outBuf strings.Builder
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_AGENT_CMD=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"TMUX=", // unset TMUX so llmux creates a fresh session
	)
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("llmux exit: %v (may be expected if tmux session exits cleanly)", err)
	}
	if sessionName := strings.TrimSpace(outBuf.String()); sessionName != "" {
		t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	}

	// Wait for the stub to write its log.
	deadline := time.Now().Add(10 * time.Second)
	var logContent string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logFile)
		if err == nil && len(data) > 0 {
			logContent = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if logContent == "" {
		t.Fatalf("stub did not write log file within 10s (marker check not executed)")
	}
	if !strings.HasPrefix(logContent, "marker_found=true args=--session-id ") {
		t.Fatalf("child started before pending marker existed, or without --session-id: %q", logContent)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var markerPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pending-") {
			markerPath = filepath.Join(stateDir, entry.Name())
			break
		}
	}
	if markerPath == "" {
		t.Fatal("pending marker was absent while the spawned child was alive")
	}
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker llmuxPendingMarker
	if err := json.Unmarshal(markerData, &marker); err != nil || marker.SpawnInstant == 0 {
		t.Fatalf("pending marker missing durable spawn instant: %v (%q)", err, markerData)
	}
	if marker.SpawnInstant > time.Now().UnixMilli() {
		t.Fatalf("pending marker spawn instant %d is in the future", marker.SpawnInstant)
	}
	if !uuidRe.MatchString(marker.SessionID) {
		t.Fatalf("pending marker carries no valid session id: %q", markerData)
	}
	if want := "marker_found=true args=--session-id " + marker.SessionID; logContent != want {
		t.Fatalf("agent was launched with a different session id than the marker records:\n got: %q\nwant: %q", logContent, want)
	}
	t.Log("✓ pending marker (with the session id) preceded the agent startup")
}

// TestLlmux_ResumeMarker verifies `llmux --resume <uuid>` launches
// `<agent> --resume <uuid>` and records the same id with resume=true, and that
// a malformed id is refused before anything is launched.
func TestLlmux_ResumeMarker(t *testing.T) {
	mustBin(t, "tmux")
	llmuxBin := mustBin(t, "llmux")
	stateDir := llmuxStateDir(t)
	logFile := filepath.Join(t.TempDir(), "resume-args.log")
	stubScript := filepath.Join(t.TempDir(), "stub-agent")
	stub := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s\\n' \"$1\" \"$2\" > %s\nsleep 1\n", shellQuoteForScript(logFile))
	if err := os.WriteFile(stubScript, []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "RELAY_AGENT_CMD="+stubScript, "RELAY_STATE_DIR="+stateDir, "TMUX=")

	bad := exec.Command(llmuxBin, "--resume", "not-a-uuid")
	bad.Env = env
	if out, err := bad.CombinedOutput(); err == nil {
		t.Fatalf("llmux accepted a malformed session id: %s", out)
	}
	if entries, _ := os.ReadDir(stateDir); len(entries) != 0 {
		t.Fatalf("a refused launch left state behind: %v", entries)
	}

	sid := testSID(60)
	cmd := exec.Command(llmuxBin, "--resume", sid)
	cmd.Env = env
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("llmux --resume: %v", err)
	}
	name := strings.TrimSpace(out.String())
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) && got == "" {
		if data, err := os.ReadFile(logFile); err == nil {
			got = strings.TrimSpace(string(data))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != "--resume "+sid {
		t.Fatalf("agent args = %q, want %q", got, "--resume "+sid)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "pending-"+name))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		SessionID string `json:"sessionID"`
		Resume    bool   `json:"resume"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.SessionID != sid || !m.Resume {
		t.Fatalf("resume marker = %q (err %v), want sessionID %s and resume=true", data, err, sid)
	}
	t.Log("✓ llmux --resume records the exact id with resume=true")
}

// TestLlmux_MarkerBeforeSpawn_SystemdScope exercises the fresh-server birth
// path inside the user scope. The scope's manager environment is not the
// llmux process environment, so this is deliberately separate from the
// general marker ordering test: the child must see the exact state directory
// containing its already-published marker.
func TestLlmux_MarkerBeforeSpawn_SystemdScope(t *testing.T) {
	mustBin(t, "tmux")
	if systemdRun, err := exec.LookPath("systemd-run"); err != nil {
		t.Skipf("systemd-run --user probe failed: %v", err)
	} else if out, err := exec.Command(systemdRun, "--user", "--scope", "--collect", "true").CombinedOutput(); err != nil {
		t.Skipf("systemd-run --user probe failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	llmuxBin := mustBin(t, "llmux")
	stateDir := llmuxStateDir(t)
	logFile := filepath.Join(t.TempDir(), "scope-marker-check.log")

	stubScript := filepath.Join(t.TempDir(), "stub-agent")
	stubContent := fmt.Sprintf(`#!/bin/sh
marker_found=false
for marker in "$RELAY_STATE_DIR"/pending-*; do
  if [ -e "$marker" ]; then marker_found=true; fi
done
printf 'marker_found=%%s state_dir=%%s home=%%s\n' "$marker_found" "$RELAY_STATE_DIR" "$HOME" > %s
sleep 1
`, shellQuoteForScript(logFile))
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	cmd := exec.Command(llmuxBin)
	var stdout, stderr strings.Builder
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_AGENT_CMD=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"TMUX=",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("llmux scope launch: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tmux isolation mode: systemd-user-scope") {
		t.Fatalf("llmux did not use forced systemd scope: %s", stderr.String())
	}
	if sessionName := strings.TrimSpace(stdout.String()); sessionName != "" {
		t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	}

	deadline := time.Now().Add(10 * time.Second)
	var logContent string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logFile); err == nil && len(data) > 0 {
			logContent = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if logContent == "" {
		t.Fatal("scope child did not write marker observation")
	}
	wantPrefix := "marker_found=true state_dir=" + stateDir + " home=" + os.Getenv("HOME")
	if logContent != wantPrefix {
		t.Fatalf("scope child did not inherit llmux marker environment: got %q, want %q", logContent, wantPrefix)
	}
}

// shellQuoteForScript quotes a literal for the POSIX shell used by marker stubs.
func shellQuoteForScript(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// TestLlmux_NewWindowMarkerBeforeSpawn verifies that llmux writes the pending marker
// BEFORE running `tmux new-window` on the inside-pane (default-socket) path.
//
// Approach:
//   - Start a real tmux session on the DEFAULT per-uid socket.
//   - Run llmux with TMUX set to look like the default socket, with a stub
//     the agent that observes the marker at its own startup.
//   - Verify the marker was already present.
func TestLlmux_NewWindowMarkerBeforeSpawn(t *testing.T) {
	mustBin(t, "tmux")
	llmuxBin := mustBin(t, "llmux")

	stateDir := llmuxStateDir(t)
	logFile := filepath.Join(t.TempDir(), "nw-marker-check.log")

	// Create a host tmux session on the default socket.
	// Ensure a previous short-lived test server cannot leave a stale socket.
	// kill-server returns before the server process has fully exited; a
	// new-session issued in that window dies with "server exited unexpectedly".
	// Wait until no server answers on the socket before creating the host.
	_ = exec.Command("tmux", "-L", "default", "kill-server").Run()
	goneDeadline := time.Now().Add(10 * time.Second)
	for exec.Command("tmux", "-L", "default", "list-sessions").Run() == nil {
		if time.Now().After(goneDeadline) {
			t.Fatalf("stale default tmux server did not exit after kill-server")
		}
		time.Sleep(50 * time.Millisecond)
	}
	hostSession := fmt.Sprintf("test-nw-host-%d", time.Now().UnixMilli())
	startCmd := exec.Command("tmux", "-L", "default", "new-session", "-d", "-s", hostSession,
		"-x", "220", "-y", "50", "sleep 60")
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Fatalf("create host tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("tmux", "-L", "default", "kill-session", "-t", hostSession).Run()
	})

	// Build a stub that records the durable marker state at its own startup.
	stubScript := filepath.Join(t.TempDir(), "stub-agent")
	stubContent := fmt.Sprintf(`#!/bin/sh
# Stub agent: observe marker state before remaining alive.
marker_found=false
for marker in "%s"/pending-*; do
  if [ -e "$marker" ]; then marker_found=true; fi
done
printf 'marker_found=%%s\n' "$marker_found" > "%s"
sleep 1
`, stateDir, logFile)
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Determine the default socket path so we can fake TMUX correctly.
	defaultSocket := fmt.Sprintf("/tmp/tmux-%d/default", os.Getuid())
	// Construct a TMUX env var that looks like we are inside the default socket.
	// Format: <socket-path>,<server-pid>,<pane-window-index>
	fakeServerPID := "99999"
	fakeTMUXEnv := defaultSocket + "," + fakeServerPID + ",0"

	// Run llmux as if inside the default socket.
	cmd := exec.Command(llmuxBin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_AGENT_CMD=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		// Set TMUX to look like we are inside the default socket so llmux takes
		// the new-window path.  extractHostSessionName queries "tmux display-message -p #S"
		// which will return the hostSession we created above.
		fmt.Sprintf("TMUX=%s", fakeTMUXEnv),
	)
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("llmux exit: %v", err)
	}
	t.Logf("llmux stdout: %q", outBuf.String())

	// Wait for the stub to write its log.
	deadline := time.Now().Add(15 * time.Second)
	var logContent string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logFile)
		if err == nil && len(data) > 0 {
			logContent = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if logContent == "" {
		t.Fatalf("stub did not write log within 15s (new-window marker check not executed)")
	}
	if logContent != "marker_found=true" {
		t.Fatalf("new-window child started before pending marker existed: %q", logContent)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var markerPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pending-") {
			markerPath = filepath.Join(stateDir, entry.Name())
			break
		}
	}
	if markerPath == "" {
		t.Fatal("pending marker was absent while the new-window child was alive")
	}
	t.Log("✓ pending marker preceded the agent startup on new-window path")

	// Clean up llm-* tmux windows in the host session.
	t.Cleanup(func() {
		if out, err := exec.Command("tmux", "-L", "default", "list-windows",
			"-t", hostSession, "-F", "#{window_name}").Output(); err == nil {
			for _, w := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(w, "llm-") {
					exec.Command("tmux", "-L", "default", "kill-window",
						"-t", hostSession+":="+w).Run()
				}
			}
		}
	})
}

// llmuxPendingMarker is the JSON structure of a pending marker file.
type llmuxPendingMarker struct {
	Target       string `json:"target"`
	SessionID    string `json:"sessionID"`
	SpawnInstant int64  `json:"spawnInstant"`
}

// llmuxStateDir stays outside /tmp because the scope used for a fresh tmux
// birth has a private /tmp; /workspace is the shared integration-test mount.
func llmuxStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/workspace", "llmux-marker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestLlmux_ForeignSocketFallsBackToDefault verifies that when llmux is invoked
// with $TMUX pointing to a non-default tmux socket (a "foreign" socket), it
// ignores that socket and creates the the agent session on the default
// per-uid socket instead.
//
// Approach:
//   - Start a second tmux server on socket name "relaytest" (-L relaytest).
//   - Set TMUX to the relaytest socket path to simulate running inside a foreign socket.
//   - Verify llmux creates the new session on the DEFAULT socket (not relaytest).
func TestLlmux_ForeignSocketFallsBackToDefault(t *testing.T) {
	mustBin(t, "tmux")
	llmuxBin := mustBin(t, "llmux")

	stateDir := t.TempDir()

	// Start a tmux server on the "relaytest" socket (separate from default).
	relaytestSession := fmt.Sprintf("test-foreign-%d", time.Now().UnixMilli())
	startCmd := exec.Command("tmux", "-L", "relaytest", "new-session", "-d",
		"-s", relaytestSession, "-x", "220", "-y", "50", "sleep 60")
	if err := startCmd.Run(); err != nil {
		t.Fatalf("create relaytest tmux session: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("tmux", "-L", "relaytest", "kill-server").Run()
	})

	// Get the socket path for the relaytest server.
	// tmux sockets are in /tmp/tmux-<uid>/ by default, named after "-L <name>".
	relaytestSocket := fmt.Sprintf("/tmp/tmux-%d/relaytest", os.Getuid())

	// Build a minimal stub the agent that exits immediately.
	stubScript := filepath.Join(t.TempDir(), "stub-agent")
	stubContent := `#!/bin/sh
# Minimal stub: just exit so tmux session ends quickly.
sleep 1
`
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Construct a TMUX env var as if running inside the relaytest socket.
	fakeServerPID := "88888"
	fakeTMUXEnv := relaytestSocket + "," + fakeServerPID + ",0"

	// Run llmux with TMUX pointing to the non-default relaytest socket.
	cmd := exec.Command(llmuxBin)
	var outBuf strings.Builder
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_AGENT_CMD=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		fmt.Sprintf("TMUX=%s", fakeTMUXEnv), // non-default socket
	)
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("llmux exit: %v", err)
	}

	sessionName := strings.TrimSpace(outBuf.String())
	if sessionName == "" {
		t.Fatalf("llmux produced no session name on stdout")
	}
	t.Logf("llmux reported session: %q", sessionName)

	t.Cleanup(func() {
		exec.Command("tmux", "-L", "default", "kill-session", "-t", sessionName).Run()
		os.Remove(filepath.Join(stateDir, "pending-"+sessionName))
	})

	// Verify the session was created on the DEFAULT socket, not relaytest.
	defaultCheck := exec.Command("tmux", "-L", "default", "has-session", "-t", sessionName)
	if err := defaultCheck.Run(); err != nil {
		t.Errorf("session %q not found on default socket: llmux may have created it on the wrong socket (err: %v)", sessionName, err)
	} else {
		t.Logf("✓ session %q exists on default socket", sessionName)
	}

	// Verify the session was NOT created on the relaytest socket.
	relaytestCheck := exec.Command("tmux", "-L", "relaytest", "has-session", "-t", sessionName)
	if relaytestCheck.Run() == nil {
		t.Errorf("session %q found on relaytest socket; llmux should have used the default socket for foreign-socket callers", sessionName)
	} else {
		t.Logf("✓ session %q not on relaytest socket (correct)", sessionName)
	}

	// Verify the pending marker was written with the session id.
	markerPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Logf("marker already cleaned up or not written: %v", err)
	} else {
		var m llmuxPendingMarker
		if jsonErr := json.Unmarshal(markerData, &m); jsonErr != nil {
			t.Errorf("pending marker is not valid JSON: %v (content: %q)", jsonErr, markerData)
		} else if !uuidRe.MatchString(m.SessionID) {
			t.Error("pending marker has no valid session id; the id is required for session binding")
		} else {
			t.Logf("✓ pending marker sessionID=%q target=%q", m.SessionID, m.Target)
		}
	}
}
