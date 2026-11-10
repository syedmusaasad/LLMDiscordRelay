// Package inject sends Discord thread replies into a live Claude Code tmux pane.
//
// Flow:
//  1. Only messages from DISCORD_USER_ID are honoured; bots and other authors are dropped.
//  2. Readiness probe: capture-pane polls for the idle TUI (see PaneReady: an
//     input line between two rules and no "esc to interrupt" footer) up to 30 s;
//     falls back to a fixed 2 s sleep if the pane never becomes ready.
//  3. Image attachments are downloaded to <stateDir>/staging/<sessionID>/ with
//     filenames sanitised of exotic whitespace.  Their local path is appended to the
//     injected text.
//  4. Single-line text goes through `tmux send-keys -l`; multi-line text is
//     pasted with load-buffer/paste-buffer -p (bracketed paste), because
//     send-keys would submit at the first newline. Enter then submits.
//  5. `!title <t>` renames the Discord thread.
package inject

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/tmuxctl"
)

const (
	// ReadyTimeout bounds how long WaitReady polls.
	ReadyTimeout       = 30 * time.Second
	readinessProbePoll = 200 * time.Millisecond
	readinessFallback  = 2 * time.Second
	pasteSettle        = 150 * time.Millisecond
)

// ThreadRenamer is an optional interface a Discord client may implement to
// rename a thread.
type ThreadRenamer interface {
	RenameThread(threadID, name string) error
}

// Injector injects text into a live tmux pane.
type Injector struct {
	StateDir string // root state dir; staging lives at <StateDir>/staging/<sessionID>/
}

// New creates an Injector.
func New(stateDir string) *Injector {
	return &Injector{StateDir: stateDir}
}

// ShouldAcceptAuthor returns true if the message author should be honoured.
// authorID is the message author's Discord user ID; allowedUserID is
// DISCORD_USER_ID from the environment.  An empty allowedUserID is
// fail-closed: every author is rejected (the relay also refuses to start).
// Bot messages (isBot=true) are always dropped.
func ShouldAcceptAuthor(authorID string, isBot bool, allowedUserID string) bool {
	if isBot {
		return false
	}
	if allowedUserID == "" {
		return false
	}
	return authorID == allowedUserID
}

// Inject performs the full inject sequence for one Discord thread reply:
//  1. Download any image attachments.
//  2. Wait for pane readiness (or fall back).
//  3. Send the (augmented) text to the pane.
//
// Parameters:
//
//	target      – tmux pane target, e.g. "mysession:0.0"
//	sessionID   – Claude Code session ID (used for staging dir)
//	content     – message text
//	attachments – list of (filename, url) pairs for image attachments
func (inj *Injector) Inject(target, sessionID, content string, attachments []Attachment) error {
	// Download attachments
	extraPaths, err := inj.downloadAttachments(sessionID, attachments)
	if err != nil {
		log.Printf("inject: attachment download partial error: %v", err)
	}

	text := joinText(content, extraPaths)

	// Readiness probe
	waitForReady(target)

	// Send to pane
	return SendText(target, text)
}

// SendPrompt downloads attachments and sends the text WITHOUT running the
// readiness probe. The caller must already have established readiness with
// WaitReady (the spawn path does, so it can tear down on probe failure).
func (inj *Injector) SendPrompt(target, sessionID, content string, attachments []Attachment) error {
	extraPaths, err := inj.downloadAttachments(sessionID, attachments)
	if err != nil {
		log.Printf("inject: attachment download partial error: %v", err)
	}
	return SendText(target, joinText(content, extraPaths))
}

func joinText(content string, extraPaths []string) string {
	text := content
	if len(extraPaths) > 0 {
		if text != "" {
			text += "\n"
		}
		text += strings.Join(extraPaths, "\n")
	}
	return text
}

// Attachment is a named URL for a file attachment.
type Attachment struct {
	Filename string
	URL      string
}

// downloadAttachments downloads each attachment into the staging dir and
// returns the list of local paths.
func (inj *Injector) downloadAttachments(sessionID string, attachments []Attachment) ([]string, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	stagingDir := filepath.Join(inj.StateDir, "staging", sessionID)
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return nil, fmt.Errorf("inject: mkdir staging: %w", err)
	}
	var paths []string
	var lastErr error
	for _, att := range attachments {
		localPath, err := downloadFile(stagingDir, att.Filename, att.URL)
		if err != nil {
			lastErr = err
			log.Printf("inject: download %s: %v", att.Filename, err)
			continue
		}
		paths = append(paths, localPath)
	}
	return paths, lastErr
}

// DownloadAttachments downloads attachments into <stateDir>/staging/<sessionID>/ and
// returns the local file paths. Exported for testing.
func (inj *Injector) DownloadAttachments(sessionID string, attachments []Attachment) ([]string, error) {
	return inj.downloadAttachments(sessionID, attachments)
}

// downloadFile downloads a URL into dir/<sanitised-filename> and returns the path.
func downloadFile(dir, filename, rawURL string) (string, error) {
	safe := sanitiseFilename(filename)
	if safe == "" {
		safe = "attachment"
	}
	dest := filepath.Join(dir, safe)

	resp, err := http.Get(rawURL) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("inject: get %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("inject: create %s: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("inject: write %s: %w", dest, err)
	}
	return dest, nil
}

// sanitiseFilename removes exotic whitespace and control characters from a filename.
func sanitiseFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		// Allow normal printable ASCII + safe unicode letters/digits/punctuation
		if r == os.PathSeparator || r == 0 {
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		// Replace any whitespace that isn't a plain space with underscore
		if unicode.IsSpace(r) && r != ' ' {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// waitForReady probes the tmux pane for the readiness signature up to 30 s;
// falls back to a fixed 2 s sleep.
func waitForReady(target string) {
	if WaitReady(target) {
		return
	}
	// Fallback
	time.Sleep(readinessFallback)
}

// WaitReady polls the pane until PaneReady, for at most 30 s. It returns false
// on timeout, or early if the pane cannot be captured for ~1 s (pane gone).
func WaitReady(target string) bool {
	deadline := time.Now().Add(ReadyTimeout)
	captureErrs := 0
	for time.Now().Before(deadline) {
		text, err := capturePaneText(target)
		if err == nil {
			captureErrs = 0
			if PaneReady(text) {
				return true
			}
		} else if captureErrs++; captureErrs >= 5 {
			return false
		}
		time.Sleep(readinessProbePoll)
	}
	return false
}

// capturePaneText captures the visible text of a tmux pane.
func capturePaneText(target string) (string, error) {
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(tmuxBin, "capture-pane", "-t", target, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

var pasteSeq atomic.Uint64

// nextBufferName returns a tmux buffer name unique per call (pid + atomic
// sequence), safe for concurrent injections.
func nextBufferName() string {
	return fmt.Sprintf("llmrelay-%d-%d", os.Getpid(), pasteSeq.Add(1))
}

// SendText submits text to the pane as one prompt. Multi-line text is pasted
// with bracketed paste so its newlines stay inside the prompt.
func SendText(target, text string) error {
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return err
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if strings.Contains(text, "\n") {
		if err := pasteBuffer(tmuxBin, target, text); err != nil {
			return err
		}
		time.Sleep(pasteSettle)
	} else {
		cmd := exec.Command(tmuxBin, "send-keys", "-t", target, "-l", text)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("inject: send-keys literal: %w: %s", err, out)
		}
		// Let the TUI consume the text before Enter arrives. Real Claude Code
		// coalesces text and an Enter that follow within a few milliseconds
		// into one paste-like chunk and then leaves the prompt typed but
		// unsubmitted (seen live on freshly started and resumed panes).
		time.Sleep(pasteSettle)
	}
	cmd2 := exec.Command(tmuxBin, "send-keys", "-t", target, "", "Enter")
	if out, err := cmd2.CombinedOutput(); err != nil {
		return fmt.Errorf("inject: send-keys enter: %w: %s", err, out)
	}
	return nil
}

// pasteBuffer loads text into a private tmux buffer and pastes it (bracketed,
// -p) into target. paste-buffer -d deletes the buffer afterwards.
func pasteBuffer(tmuxBin, target, text string) error {
	f, err := os.CreateTemp("", "llm-relay-paste-*")
	if err != nil {
		return fmt.Errorf("inject: paste temp file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return fmt.Errorf("inject: paste temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	name := nextBufferName()
	if out, err := exec.Command(tmuxBin, "load-buffer", "-b", name, f.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("inject: load-buffer: %w: %s", err, out)
	}
	if out, err := exec.Command(tmuxBin, "paste-buffer", "-p", "-d", "-b", name, "-t", target).CombinedOutput(); err != nil {
		_ = exec.Command(tmuxBin, "delete-buffer", "-b", name).Run()
		return fmt.Errorf("inject: paste-buffer: %w: %s", err, out)
	}
	return nil
}

// HandleTitleCommand checks if content is a !title <t> command and returns the
// title and true if so, or ("", false) otherwise.
func HandleTitleCommand(content string) (title string, ok bool) {
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "!title ") {
		return "", false
	}
	t := strings.TrimSpace(strings.TrimPrefix(s, "!title "))
	if t == "" {
		return "", false
	}
	return t, true
}
