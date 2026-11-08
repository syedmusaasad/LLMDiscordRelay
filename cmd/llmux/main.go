// cmd/llmux: launch Claude Code in a named tmux session so the relay can mirror it.
//
// Usage:
//
//	llmux [args...]         — launch a new session named llm-<unixtime>-<pid>
//	llmux --resume <uuid>   — resume an existing Claude Code session
//	llmux ls                — list active relay sessions
//	llmux update            — update to the latest release
//	llmux uninstall         — remove the installed relay
//
// A new session is `claude --session-id <uuid>` with a uuid4 chosen here; a
// resume is `claude --resume <uuid>`. llmux writes
// <state dir>/pending-<session-name> as JSON containing the tmux target, the
// spawn instant and that session id BEFORE the process starts. Claude Code
// creates the transcript only when the first prompt is submitted; the relay
// pairs the marker with the transcript of exactly that id (see
// docs/BINDING-PROTOCOL.md).
//
// Inside-pane detection:
//   - If we are already inside a pane on the DEFAULT per-uid socket
//     (/tmp/tmux-<uid>/default or $TMUX_TMPDIR/default) → new-window in
//     the host session, target <host-session>:=<window-name>
//   - Any other socket (including non-default $TMUX) → unset TMUX and run
//     tmux -L default new-session on the default socket.
//
// Env overrides:
//
//	RELAY_AGENT_CMD        agent command, split on whitespace; the first word is
//	                       resolved on PATH (default: "claude"; e.g. "pl")
//	RELAY_STATE_DIR        state dir (default: ~/.llm-discord-relay)
//	RELAY_CLAUDE_PROJECTS  Claude Code projects dir (forwarded to the agent
//	                       process; the relay reads transcripts from it)
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/tmuxctl"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "llmux:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Handle special subcommands
	if len(args) == 1 && args[0] == "ls" {
		return runLS()
	}
	if len(args) == 1 && args[0] == "update" {
		return runUpdate()
	}
	if len(args) == 1 && args[0] == "uninstall" {
		return runUninstall()
	}

	return runLaunch(args)
}

func runUpdate() error {
	root, err := updateSource()
	if err != nil {
		return err
	}
	// Releases are pinned by the latest tag, not the checkout's current branch.
	// --force lets a moved remote latest tag replace the local one.
	commands := [][]string{{"fetch", "--tags", "--force"}, {"checkout", "--detach", "latest"}}
	for _, args := range commands {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	cmd := exec.Command("make", "build")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	if err := installUpdatedBinaries(root); err != nil {
		return err
	}
	promptRestart()
	return nil
}

func installUpdatedBinaries(root string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	destDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	for _, name := range []string{"relay", "llmux"} {
		data, err := os.ReadFile(filepath.Join(root, "bin", name))
		if err != nil {
			return fmt.Errorf("read rebuilt %s: %w", name, err)
		}
		if err := replaceFile(filepath.Join(destDir, name), data, 0755); err != nil {
			return fmt.Errorf("install rebuilt %s: %w", name, err)
		}
	}
	return nil
}

// replaceFile writes data to a temp file in dest's directory, chmods it and
// renames it over dest. Rename swaps the directory entry instead of opening the
// old inode for writing, so replacing a binary that is currently running does
// not fail with ETXTBSY and running processes keep their old inode.
func replaceFile(dest string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".new-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	ok = true
	return nil
}

// updateSource reads the checkout recorded by install.sh. If an installed
// binary has no usable source, clone a fresh working tree instead of depending
// on the caller's current directory.
func updateSource() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	marker := filepath.Join(home, ".config", "llm-discord-relay", "install-source")
	if data, readErr := os.ReadFile(marker); readErr == nil {
		root := strings.TrimSpace(string(data))
		if root != "" {
			if _, statErr := os.Stat(filepath.Join(root, ".git")); statErr == nil {
				return root, nil
			}
		}
	}
	root := filepath.Join(home, ".cache", "llm-discord-relay", "source")
	if _, statErr := os.Stat(filepath.Join(root, ".git")); statErr != nil {
		if err := os.MkdirAll(filepath.Dir(root), 0755); err != nil {
			return "", err
		}
		cmd := exec.Command("git", "clone", "https://github.com/syedmusaasad/LLMDiscordRelay.git", root)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("clone update source: %w", err)
		}
	}
	return root, nil
}

func promptRestart() {
	const instruction = "Update complete. Restart with: systemctl --user restart llm-discord-relay"
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintln(os.Stderr, instruction)
		return
	}
	fmt.Fprint(os.Stderr, instruction+" now? [y/N] ")
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		_ = exec.Command("systemctl", "--user", "restart", "llm-discord-relay").Run()
	}
}

func runUninstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configDir := filepath.Join(home, ".config", "llm-discord-relay")
	envPath := filepath.Join(configDir, ".env")
	if data, err := os.ReadFile(envPath); err == nil {
		backup := envPath + ".uninstall.bak"
		if err := os.WriteFile(backup, data, 0600); err != nil {
			return fmt.Errorf("backup .env: %w", err)
		}
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "llm-discord-relay.service").Run()
	_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", "llm-discord-relay.service"))
	_ = os.Remove(filepath.Join(home, ".local", "bin", "relay"))
	_ = os.Remove(filepath.Join(home, ".local", "bin", "llmux"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Fprintln(os.Stderr, "Uninstalled; .env backup retained at "+envPath+".uninstall.bak")
	return nil
}

func repoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locate repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runLS lists active relay (llm-*) sessions and their windows.
// Window lines that duplicate their session name are suppressed.
func runLS() error {
	// List all sessions, filter to llm-* only.
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return err
	}
	sessOut, err := exec.Command(tmuxBin, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		// No sessions running is OK.
		return nil
	}
	var llmSessions []string
	for _, line := range strings.Split(strings.TrimSpace(string(sessOut)), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "llm-") {
			llmSessions = append(llmSessions, line)
		}
	}
	if len(llmSessions) == 0 {
		return nil
	}

	for _, sess := range llmSessions {
		fmt.Println(sess)
		// List windows for this session; suppress windows whose name equals the session name.
		winOut, err := exec.Command(tmuxBin, "list-windows", "-t", sess, "-F", "#{window_name}").Output()
		if err != nil {
			continue
		}
		for _, wline := range strings.Split(strings.TrimSpace(string(winOut)), "\n") {
			if wline == "" || wline == sess {
				continue
			}
			fmt.Println("  " + wline)
		}
	}
	return nil
}

// pendingMarker is the JSON written to pending-<session-name> in stateDir.
// The relay reads this to pair the launch with its transcript. The session id
// is chosen by llmux (claude --session-id / --resume), so the marker carries the
// identity directly and nothing has to be inferred later.
type pendingMarker struct {
	Target       string `json:"target"`           // tmux target (e.g. "llm-123:0.0")
	SpawnInstant int64  `json:"spawnInstant"`     // time.Now().UnixMilli() when marker was written
	SessionID    string `json:"sessionID"`        // Claude Code session id of this launch
	Resume       bool   `json:"resume,omitempty"` // true for `--resume <id>`: the transcript already exists
}

// launchPlan is what llmux will run for one invocation.
type launchPlan struct {
	SessionID string
	Resume    bool
	Args      []string // arguments for the agent, identity flag first
}

// newSessionID returns a random RFC 4122 version-4 UUID.
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("llmux: no randomness: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// planLaunch turns llmux's arguments into the agent arguments. `--resume <uuid>`
// resumes; anything else is passed through to a new `--session-id <uuid>` launch.
func planLaunch(args []string, newID func() string) (launchPlan, error) {
	var rest []string
	resumeID := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--resume" || args[i] == "-r":
			if i+1 >= len(args) {
				return launchPlan{}, fmt.Errorf("--resume needs a session id")
			}
			i++
			resumeID = args[i]
		case strings.HasPrefix(args[i], "--resume="):
			resumeID = strings.TrimPrefix(args[i], "--resume=")
		default:
			rest = append(rest, args[i])
		}
	}
	if resumeID != "" {
		if !transcript.ValidSessionID(resumeID) {
			return launchPlan{}, fmt.Errorf("invalid session id %q (want a UUID)", resumeID)
		}
		return launchPlan{SessionID: resumeID, Resume: true, Args: append([]string{"--resume", resumeID}, rest...)}, nil
	}
	id := newID()
	return launchPlan{SessionID: id, Args: append([]string{"--session-id", id}, rest...)}, nil
}

// runLaunch launches a new (or resumed) Claude Code session.
func runLaunch(args []string) error {
	stateDir, err := resolveStateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	plan, err := planLaunch(args, newSessionID)
	if err != nil {
		return err
	}

	// Auto-generate session name
	now := time.Now().UnixMilli()
	pid := os.Getpid()
	sessionName := fmt.Sprintf("llm-%d-%d", now, pid)

	agent, err := resolveAgentCmd()
	if err != nil {
		return err
	}
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[llmux] tmux isolation mode: %s\n", tmuxctl.IsolationMode())

	// Detect if we're inside a tmux pane on the DEFAULT per-uid socket.
	// The TMUX env var format is: <socket-path>,<server-pid>,<window-index>
	// Only use new-window when the socket path is the default per-uid socket
	// (/tmp/tmux-<uid>/default or $TMUX_TMPDIR/default).
	// When running on a non-default socket (foreign socket), unset TMUX and
	// launch a fresh session on the default socket so all relay sessions land
	// on a predictable socket.
	isInsideDefaultSocket := isOnDefaultTmuxSocket()

	var tmuxTarget string

	if isInsideDefaultSocket {
		// We're inside tmux on the default socket; create a new window in the host session.
		hostSessionName := extractHostSessionName()
		if hostSessionName != "" {
			windowName := sessionName
			tmuxTarget = hostSessionName + ":=" + windowName

			// Write the pending marker BEFORE spawning so the relay can pair
			// the session as soon as its first prompt creates the transcript.
			pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
			marker := pendingMarker{
				Target:       tmuxTarget,
				SpawnInstant: time.Now().UnixMilli(),
				SessionID:    plan.SessionID,
				Resume:       plan.Resume,
			}
			if err := writePendingMarker(pendingPath, marker); err != nil {
				return fmt.Errorf("write pending marker: %w", err)
			}

			launchCmd := buildAgentCmd(agent, plan.Args)
			launch, _ := tmuxctl.NewBirthCommand(tmuxBin, "new-window", "-t", hostSessionName, "-n", windowName,
				"-d", launchCmd)
			launchErr := launch.Run()
			if launchErr == nil {
				fmt.Fprintf(os.Stderr, "[llmux] session %q started, target=%s\n", sessionName, tmuxTarget)
				fmt.Println(sessionName)
				return nil
			}
			// new-window failed; fall through to fresh-session path below.
			// Remove marker so it can be rewritten with the correct target.
			os.Remove(pendingPath)
		}
		// Could not get host session name or new-window failed; fall through.
	}

	// Fresh session on the default socket.
	// When running on a non-default $TMUX socket (foreign socket), we deliberately
	// ignore TMUX so tmux creates the session on the default per-uid socket.
	tmuxTarget = sessionName + ":0.0"

	// Write pending marker BEFORE spawning.
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	marker := pendingMarker{
		Target:       tmuxTarget,
		SpawnInstant: time.Now().UnixMilli(),
		SessionID:    plan.SessionID,
		Resume:       plan.Resume,
	}
	if err := writePendingMarker(pendingPath, marker); err != nil {
		return fmt.Errorf("write pending marker: %w", err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		return fmt.Errorf("verify pending marker: %w", err)
	}

	launchCmd := buildAgentCmd(agent, plan.Args)
	// Select the default socket explicitly. Merely clearing the client identity
	// is insufficient: tmux can still derive a foreign socket from the inherited
	// client environment. -L default continues to honor TMUX_TMPDIR.
	launchCmdExec, _ := tmuxctl.NewBirthCommand(tmuxBin, "-L", "default", "new-session", "-d", "-s", sessionName,
		"-x", "220", "-y", "50", launchCmd)
	launchErr := launchCmdExec.Run()
	if launchErr != nil {
		os.Remove(pendingPath)
		return fmt.Errorf("tmux new-session: %w", launchErr)
	}

	fmt.Fprintf(os.Stderr, "[llmux] session %q started, target=%s\n", sessionName, tmuxTarget)
	fmt.Println(sessionName)
	return nil
}

// writePendingMarker durably publishes the marker before the tmux child is
// allowed to start, including when that child is born in a systemd user scope.
func writePendingMarker(path string, marker pendingMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	// Publish atomically: a child born immediately after this function returns
	// must see either the old complete marker or this complete marker, never a
	// newly-created empty/truncated file.  The file and parent directory fsyncs
	// make the write durable before tmux is allowed to birth the child.
	tmpPath := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTemp = false
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// forwardedEnv are the variables copied into the agent's command line (the
// tmux server's stored environment may be stale), besides FAKE_CLAUDE_* knobs.
var forwardedEnv = []string{
	"RELAY_STATE_DIR",
	"RELAY_CLAUDE_PROJECTS",
	"CLAUDE_CONFIG_DIR",
}

// buildAgentCmd constructs the shell command string for tmux: an "env K=V ..."
// prefix forwarding the variables the agent needs but may not inherit through
// the tmux server, then the quoted agent command and arguments. Nothing is
// redirected or hidden: a launch failure is visible in the pane and exit status.
func buildAgentCmd(agent []string, args []string) string {
	var envPairs []string
	for _, key := range forwardedEnv {
		if v := os.Getenv(key); v != "" {
			envPairs = append(envPairs, key+"="+shellQuote(v))
		}
	}
	var fake []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FAKE_CLAUDE_") {
			fake = append(fake, kv)
		}
	}
	sort.Strings(fake)
	for _, kv := range fake {
		k, v, _ := strings.Cut(kv, "=")
		if v != "" {
			envPairs = append(envPairs, k+"="+shellQuote(v))
		}
	}

	parts := make([]string, 0, len(agent)+len(args)+len(envPairs)+1)
	if len(envPairs) > 0 {
		parts = append(parts, "env")
		parts = append(parts, envPairs...)
	}
	for _, a := range agent {
		parts = append(parts, shellQuote(a))
	}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps a string in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// isOnDefaultTmuxSocket returns true if the current process is running inside
// a tmux pane on the default per-uid socket.
// The default socket path is /tmp/tmux-<uid>/default (or $TMUX_TMPDIR/default).
// TMUX env var format: <socket-path>,<server-pid>,<window-index>
func isOnDefaultTmuxSocket() bool {
	tmuxEnv := os.Getenv("TMUX")
	if tmuxEnv == "" {
		return false
	}
	// Extract the socket path (first comma-separated field).
	socketPath := tmuxEnv
	if idx := strings.Index(tmuxEnv, ","); idx >= 0 {
		socketPath = tmuxEnv[:idx]
	}
	// Determine the expected default socket path.
	defaultSocket := defaultTmuxSocket()
	return socketPath == defaultSocket
}

// defaultTmuxSocket returns the path to the default per-uid tmux socket.
func defaultTmuxSocket() string {
	// $TMUX_TMPDIR/default, or /tmp/tmux-<uid>/default
	if dir := os.Getenv("TMUX_TMPDIR"); dir != "" {
		return filepath.Join(dir, "default")
	}
	return fmt.Sprintf("/tmp/tmux-%d/default", os.Getuid())
}

// extractHostSessionName gets the session name from the TMUX env var.
// TMUX=/tmp/tmux-UID/default,<server-pid>,<pane-window-index>
func extractHostSessionName() string {
	tmuxEnv := os.Getenv("TMUX")
	if tmuxEnv == "" {
		return ""
	}
	// Ask tmux for the current session name
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return ""
	}
	out, err := exec.Command(tmuxBin, "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveAgentCmd returns the agent command: $RELAY_AGENT_CMD (default
// "claude") split on whitespace, with its first word resolved on PATH.
func resolveAgentCmd() ([]string, error) {
	spec := strings.TrimSpace(os.Getenv("RELAY_AGENT_CMD"))
	if spec == "" {
		spec = "claude"
	}
	words := strings.Fields(spec)
	bin, err := exec.LookPath(words[0])
	if err != nil {
		return nil, fmt.Errorf("agent command %q not found on PATH (set RELAY_AGENT_CMD): %w", words[0], err)
	}
	words[0] = bin
	return words, nil
}

// resolveStateDir returns the state directory.
func resolveStateDir() (string, error) {
	if v := os.Getenv("RELAY_STATE_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".llm-discord-relay"), nil
}
