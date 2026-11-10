// cmd/fake-claude: test double for the Claude Code 2.1.319 terminal UI.
//
// It reproduces the parts of Claude Code the relay depends on:
//   - `--session-id <uuid>` starts a session, `--resume <uuid>` resumes one; an
//     unknown --resume id prints "No conversation found with session ID: <id>"
//     and exits 1, like the real CLI.
//   - FAKE_CLAUDE_UNTRUSTED=1 shows the folder-trust dialog first ("❯ No, exit"
//     selected; Down + Enter trusts; Esc or "No" exits 1).
//   - The idle pane is an input line "❯ " between two "────" rules; the footer
//     says "esc to interrupt" while a turn runs.
//   - Permission prompts ("Do you want to proceed?") and the AskUserQuestion
//     picker are rendered as the real TUI draws them and answered with the same
//     keys (1 / Esc, Down+Enter).
//   - The transcript is appended to <projects>/<project-slug>/<session-id>.jsonl
//     in the real line shapes (testdata/claude), created when the first prompt
//     is submitted, not at launch.
//
// The projects directory is $RELAY_CLAUDE_PROJECTS, else $CLAUDE_CONFIG_DIR/projects,
// else ~/.claude/projects.
//
// Script turns with FAKE_CLAUDE_SCRIPT (path to a JSON array):
//
//	[{"prompt": "ping", "reply": "Pong.", "permissions": 1, "question": true}, ...]
//
// "permissions" (default 0) and "question" are optional. Other knobs:
//
//	FAKE_CLAUDE_UNTRUSTED=1             show the trust dialog first
//	FAKE_CLAUDE_READY_DELAY_MS=<n>      delay before the idle pane appears
//	FAKE_CLAUDE_SWALLOW_UNTIL_READY=1   discard keys typed before the idle pane
//	FAKE_CLAUDE_TRANSCRIPT_DELAY_MS=<n> delay before the transcript file is created
//	FAKE_CLAUDE_REFUSE_FIRST_RESUME=1   refuse every --resume (even a known id)
//	FAKE_CLAUDE_DROP_FIRST_PROMPT=1     lose the first submitted prompt (nothing is
//	                                    shown or recorded), like a keystroke eaten
//	                                    by a TUI that was not listening yet
//	FAKE_CLAUDE_PERMISSION_PROMPT=1     one permission prompt in the first turn
//	FAKE_CLAUDE_PERMISSION_PROMPTS=<n>  n permission prompts in the first turn
//	FAKE_CLAUDE_QUESTION_PICKER=1       an AskUserQuestion picker in the first turn
//	FAKE_CLAUDE_QUESTION_OPTIONS=a,b    picker options (default Red,Green)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

const (
	version   = "2.1.319"
	modelName = "claude-fake-1"
	rule      = "────────────────────────────────────────────────────────────────────────"
	dashRule  = "╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌"
)

// scriptTurn is one entry in FAKE_CLAUDE_SCRIPT.
type scriptTurn struct {
	Prompt      string `json:"prompt"`
	Reply       string `json:"reply"`
	Permissions int    `json:"permissions"`
	Question    bool   `json:"question"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ── pure renderers (unit-tested against internal/inject's parsers) ──────────

func header(cwd string) string {
	return " ▐▛███▜▌   Claude Code v" + version + " (fake)\n" +
		"▝▜█████▛▘  " + modelName + " · Claude Max\n" +
		"  ▘▘ ▝▝    " + cwd + "\n"
}

func renderIdle(cwd string, history []string, typed string, busy bool) string {
	footer := "  ? for shortcuts"
	if busy {
		footer = "  esc to interrupt"
	}
	var b strings.Builder
	b.WriteString(header(cwd))
	b.WriteString("\n")
	for _, h := range tail(history, 16) {
		b.WriteString(h + "\n")
	}
	b.WriteString("\n" + rule + "\n")
	b.WriteString("❯ " + typed + "\n")
	b.WriteString(rule + "\n" + footer + "\n")
	return b.String()
}

func renderTrust(cwd string, sel int) string {
	row := func(i int, label string) string {
		if i == sel {
			return " ❯ " + label
		}
		return "   " + label
	}
	return " Accessing workspace:\n\n " + cwd + "\n\n" +
		" Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.\n\n" +
		row(0, "No, exit") + "\n" + row(1, "Yes, I trust this folder") + "\n\n" +
		" Enter to confirm · Esc to cancel\n"
}

func renderPermission(cwd string, history []string, command, description string, sel int) string {
	row := func(i int, label string) string {
		if i == sel {
			return " ❯ " + label
		}
		return "   " + label
	}
	var b strings.Builder
	b.WriteString(header(cwd))
	b.WriteString("\n")
	for _, h := range tail(history, 10) {
		b.WriteString(h + "\n")
	}
	b.WriteString("\n" + rule + "\n")
	b.WriteString(" Bash command\n")
	b.WriteString(dashRule + "\n")
	b.WriteString("   " + command + "\n")
	b.WriteString("   " + description + "\n")
	b.WriteString(dashRule + "\n")
	b.WriteString(" Do you want to proceed?\n")
	b.WriteString(row(0, "1. Yes") + "\n")
	b.WriteString(row(1, "2. Yes, and always allow access to "+filepath.Base(cwd)+"/ from this project") + "\n")
	b.WriteString(row(2, "3. No") + "\n\n")
	b.WriteString(" Esc to cancel · Tab to amend\n")
	return b.String()
}

func renderQuestion(cwd string, history []string, headerText, question string, options []string, sel int) string {
	var b strings.Builder
	b.WriteString(header(cwd))
	b.WriteString("\n")
	for _, h := range tail(history, 8) {
		b.WriteString(h + "\n")
	}
	b.WriteString("\n" + rule + "\n")
	b.WriteString("☐ " + headerText + "\n\n" + question + "\n\n")
	mark := func(i int) string {
		if i == sel {
			return "❯ "
		}
		return "  "
	}
	for i, o := range options {
		b.WriteString(fmt.Sprintf("%s%d. %s\n     Choose %s\n", mark(i), i+1, o, o))
	}
	b.WriteString(fmt.Sprintf("%s%d. Type something.\n", mark(len(options)), len(options)+1))
	b.WriteString(rule + "\n  Chat about this\n\n")
	b.WriteString("Enter to select · ↑/↓ to navigate · Esc to cancel\n")
	return b.String()
}

func tail(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// projectSlug is how Claude Code names a project directory under projects/.
func projectSlug(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, cwd)
}

// ── keys ────────────────────────────────────────────────────────────────────

// keyReader turns raw terminal bytes into key tokens: "Enter", "Escape", "Up",
// "Down", "EOF", "paste:<text>" or a single typed character.
type keyReader struct {
	bytes chan byte
}

func newKeyReader(f *os.File) *keyReader {
	kr := &keyReader{bytes: make(chan byte, 4096)}
	go func() {
		r := bufio.NewReader(f)
		for {
			b, err := r.ReadByte()
			if err != nil {
				close(kr.bytes)
				return
			}
			kr.bytes <- b
		}
	}()
	return kr
}

func (kr *keyReader) next(wait time.Duration) (byte, bool) {
	if wait <= 0 {
		b, ok := <-kr.bytes
		return b, ok
	}
	select {
	case b, ok := <-kr.bytes:
		return b, ok
	case <-time.After(wait):
		return 0, false
	}
}

func (kr *keyReader) key() string {
	b, ok := kr.next(0)
	if !ok {
		return "EOF"
	}
	switch {
	case b == 0x1b:
		n, ok := kr.next(40 * time.Millisecond)
		if !ok {
			return "Escape"
		}
		switch n {
		case '[':
			var seq []byte
			for {
				c, ok := kr.next(100 * time.Millisecond)
				if !ok {
					return "Escape"
				}
				seq = append(seq, c)
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			switch string(seq) {
			case "A":
				return "Up"
			case "B":
				return "Down"
			case "200~":
				return kr.paste()
			}
			return "?"
		case 'O':
			c, _ := kr.next(100 * time.Millisecond)
			switch c {
			case 'A':
				return "Up"
			case 'B':
				return "Down"
			}
			return "?"
		}
		return "Escape"
	case b == '\r' || b == '\n':
		return "Enter"
	case b == 0x03 || b == 0x04:
		return "EOF"
	case b < 0x20:
		return "?"
	case b < 0x80:
		return string(rune(b))
	}
	buf := []byte{b}
	for !utf8.FullRune(buf) {
		c, ok := kr.next(100 * time.Millisecond)
		if !ok {
			break
		}
		buf = append(buf, c)
	}
	return string(buf)
}

// paste reads a bracketed paste up to ESC[201~. CR and CRLF become LF.
func (kr *keyReader) paste() string {
	var buf []byte
	const end = "\x1b[201~"
	for {
		c, ok := kr.next(2 * time.Second)
		if !ok {
			break
		}
		buf = append(buf, c)
		if strings.HasSuffix(string(buf), end) {
			buf = buf[:len(buf)-len(end)]
			break
		}
	}
	s := strings.ReplaceAll(strings.ReplaceAll(string(buf), "\r\n", "\n"), "\r", "\n")
	return "paste:" + s
}

// ── session ─────────────────────────────────────────────────────────────────

type session struct {
	id       string
	cwd      string
	root     string
	path     string // transcript path once known
	history  []string
	lastUUID string
	keys     *keyReader
	counter  int
	script   []scriptTurn
	turnIdx  int
	dropped  bool // the FAKE_CLAUDE_DROP_FIRST_PROMPT prompt has been lost
}

func (s *session) draw(frame string) { fmt.Print("\x1b[2J\x1b[H" + frame) }

func run(args []string) error {
	var sessionID string
	resume := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session-id":
			if i+1 < len(args) {
				i++
				sessionID = args[i]
			}
		case "--resume", "-r":
			if i+1 < len(args) {
				i++
				sessionID = args[i]
				resume = true
			}
		case "--version", "-v":
			fmt.Println(version + " (Claude Code)")
			return nil
		}
	}
	if sessionID == "" {
		sessionID = newUUID()
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/tmp"
	}
	root, err := transcript.DefaultRoot()
	if err != nil {
		return err
	}
	s := &session{id: sessionID, cwd: cwd, root: root}

	if resume {
		matches, _ := filepath.Glob(filepath.Join(root, "*", sessionID+".jsonl"))
		if os.Getenv("FAKE_CLAUDE_REFUSE_FIRST_RESUME") == "1" || len(matches) == 0 || !transcript.ValidSessionID(sessionID) {
			fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", sessionID)
			if d := envMS("FAKE_CLAUDE_REFUSE_LINGER_MS"); d > 0 {
				time.Sleep(d)
			}
			os.Exit(1)
		}
		s.path = matches[0]
	}

	if sf := os.Getenv("FAKE_CLAUDE_SCRIPT"); sf != "" {
		data, err := os.ReadFile(sf)
		if err != nil {
			return fmt.Errorf("fake-claude: read script: %w", err)
		}
		if err := json.Unmarshal(data, &s.script); err != nil {
			return fmt.Errorf("fake-claude: parse script: %w", err)
		}
	}

	// Non-canonical, no-echo input so single keys (Esc, Down) arrive at once.
	stty := exec.Command("stty", "-icanon", "-echo", "min", "1")
	stty.Stdin = os.Stdin
	_ = stty.Run()
	fmt.Print("\x1b[?2004h") // the app asks for bracketed paste, as the real TUI does
	s.keys = newKeyReader(os.Stdin)

	if os.Getenv("FAKE_CLAUDE_UNTRUSTED") == "1" {
		if !s.trustDialog() {
			return nil
		}
	}

	if d := envMS("FAKE_CLAUDE_READY_DELAY_MS"); d > 0 {
		s.draw("Starting Claude Code…\n")
		if os.Getenv("FAKE_CLAUDE_SWALLOW_UNTIL_READY") == "1" {
			end := time.Now().Add(d)
			for time.Now().Before(end) {
				s.keys.next(50 * time.Millisecond)
			}
		} else {
			time.Sleep(d)
		}
	}

	typed := ""
	for {
		shown := strings.ReplaceAll(typed, "\n", " ⏎ ")
		s.draw(renderIdle(cwd, s.history, shown, false))
		switch k := s.keys.key(); {
		case k == "EOF":
			return nil
		case k == "Enter":
			if strings.TrimSpace(typed) == "" {
				continue
			}
			prompt := typed
			typed = ""
			if err := s.turn(prompt); err != nil {
				return err
			}
		case k == "Escape":
			typed = ""
		case strings.HasPrefix(k, "paste:"):
			typed += strings.TrimPrefix(k, "paste:")
		case len(k) > 0 && k != "?" && k != "Up" && k != "Down":
			typed += k
		}
	}
}

// trustDialog shows the folder-trust dialog. It returns true once the user
// trusts the folder; "No, exit" or Esc exits 1 like the real CLI.
func (s *session) trustDialog() bool {
	sel := 0
	for {
		s.draw(renderTrust(s.cwd, sel))
		switch s.keys.key() {
		case "Down":
			sel = 1
		case "Up":
			sel = 0
		case "Enter":
			if sel == 1 {
				return true
			}
			os.Exit(1)
		case "Escape", "EOF":
			os.Exit(1)
		}
	}
}

// turn runs one scripted turn for prompt.
func (s *session) turn(prompt string) error {
	if os.Getenv("FAKE_CLAUDE_DROP_FIRST_PROMPT") == "1" && !s.dropped {
		s.dropped = true
		return nil
	}
	started := time.Now()
	if d := envMS("FAKE_CLAUDE_TRANSCRIPT_DELAY_MS"); d > 0 && s.path == "" {
		time.Sleep(d)
	}
	var t scriptTurn
	if s.turnIdx < len(s.script) {
		t = s.script[s.turnIdx]
	} else {
		t.Reply = fmt.Sprintf("(ack turn %d)", s.turnIdx+1)
	}
	if s.turnIdx == 0 {
		if os.Getenv("FAKE_CLAUDE_PERMISSION_PROMPT") == "1" && t.Permissions == 0 {
			t.Permissions = 1
		}
		if n := int(envMS("FAKE_CLAUDE_PERMISSION_PROMPTS") / time.Millisecond); n > 0 {
			t.Permissions = n
		}
		if os.Getenv("FAKE_CLAUDE_QUESTION_PICKER") == "1" {
			t.Question = true
		}
	}
	s.turnIdx++

	if err := s.writeUser(prompt); err != nil {
		return err
	}
	s.history = append(s.history, "> "+strings.ReplaceAll(prompt, "\n", " ⏎ "))
	s.draw(renderIdle(s.cwd, s.history, "", true))
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < t.Permissions; i++ {
		ok, err := s.permission(i + 1)
		if err != nil {
			return err
		}
		if !ok {
			return s.interrupted("for tool use")
		}
	}
	if t.Question {
		ok, err := s.question()
		if err != nil {
			return err
		}
		if !ok {
			return s.interrupted("for tool use")
		}
	}

	if err := s.writeAssistantText(t.Reply); err != nil {
		return err
	}
	s.history = append(s.history, "● "+strings.ReplaceAll(t.Reply, "\n", "\n  "))
	return s.writeTurnDuration(started)
}

func (s *session) permission(n int) (bool, error) {
	toolID := s.newID("toolu_fake")
	command := "ls"
	if n > 1 {
		command = fmt.Sprintf("ls # %d", n)
	}
	desc := "List directory contents"
	if err := s.writeAssistantTool(toolID, "Bash", map[string]any{"command": command, "description": desc}); err != nil {
		return false, err
	}
	sel := 0
	for {
		s.draw(renderPermission(s.cwd, s.history, command, desc, sel))
		switch s.keys.key() {
		case "1":
			return true, s.writeToolResult(toolID, "file1\nfile2", false)
		case "2":
			return true, s.writeToolResult(toolID, "file1\nfile2", false)
		case "Down":
			if sel < 2 {
				sel++
			}
		case "Up":
			if sel > 0 {
				sel--
			}
		case "Enter":
			if sel < 2 {
				return true, s.writeToolResult(toolID, "file1\nfile2", false)
			}
			return false, s.writeToolResult(toolID, rejectedText, true)
		case "3", "Escape":
			return false, s.writeToolResult(toolID, rejectedText, true)
		case "EOF":
			os.Exit(0)
		}
	}
}

const rejectedText = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed."

func (s *session) question() (bool, error) {
	options := []string{"Red", "Green"}
	if v := os.Getenv("FAKE_CLAUDE_QUESTION_OPTIONS"); v != "" {
		options = strings.Split(v, ",")
	}
	toolID := s.newID("toolu_fake")
	input := map[string]any{"questions": []map[string]any{{"question": "Which color do you prefer?", "header": "Color", "options": options}}}
	if err := s.writeAssistantTool(toolID, "AskUserQuestion", input); err != nil {
		return false, err
	}
	sel := 0
	for {
		s.draw(renderQuestion(s.cwd, s.history, "Color", "Which color do you prefer?", options, sel))
		switch s.keys.key() {
		case "Down":
			if sel < len(options) {
				sel++
			}
		case "Up":
			if sel > 0 {
				sel--
			}
		case "Enter":
			if sel >= len(options) {
				continue // "Type something." is not a real option here
			}
			s.history = append(s.history, "  ⎿ answered: "+options[sel])
			return true, s.writeToolResult(toolID, "User has answered your questions: "+options[sel], false)
		case "Escape":
			return false, s.writeToolResult(toolID, rejectedText, true)
		case "EOF":
			os.Exit(0)
		}
	}
}

// interrupted ends a turn the way Claude Code does when the user rejects a tool.
// Interrupts get no turn_duration record: the interrupt line itself ends the turn.
func (s *session) interrupted(why string) error {
	if err := s.writeLine(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "[Request interrupted by user " + why + "]"}}},
	}); err != nil {
		return err
	}
	s.history = append(s.history, "  ⎿ Interrupted")
	return nil
}

// ── transcript writing ──────────────────────────────────────────────────────

func (s *session) newID(prefix string) string {
	s.counter++
	return fmt.Sprintf("%s_%d%04d", prefix, time.Now().UnixNano(), s.counter)
}

func newUUID() string {
	b := make([]byte, 16)
	f, err := os.Open("/dev/urandom")
	if err == nil {
		_, _ = f.Read(b)
		f.Close()
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// writeLine appends one transcript line. The file and its project directory are
// created on the first call, i.e. when the first prompt is submitted.
func (s *session) writeLine(l map[string]any) error {
	if s.path == "" {
		dir := filepath.Join(s.root, projectSlug(s.cwd))
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		s.path = filepath.Join(dir, s.id+".jsonl")
	}
	uuid := newUUID()
	l["parentUuid"] = nilIfEmpty(s.lastUUID)
	l["isSidechain"] = false
	l["uuid"] = uuid
	l["timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	l["cwd"] = s.cwd
	l["sessionId"] = s.id
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	s.lastUUID = uuid
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *session) writeUser(prompt string) error {
	return s.writeLine(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": prompt},
	})
}

func (s *session) assistant(content []map[string]any, stop string) map[string]any {
	return map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model": modelName, "id": s.newID("msg_fake"), "role": "assistant",
			"content": content, "stop_reason": stop,
		},
	}
}

func (s *session) writeAssistantText(text string) error {
	return s.writeLine(s.assistant([]map[string]any{{"type": "text", "text": text}}, "end_turn"))
}

func (s *session) writeAssistantTool(id, name string, input map[string]any) error {
	return s.writeLine(s.assistant([]map[string]any{{"type": "tool_use", "id": id, "name": name, "input": input}}, "tool_use"))
}

func (s *session) writeToolResult(id, content string, isError bool) error {
	return s.writeLine(map[string]any{
		"type": "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{
			{"tool_use_id": id, "type": "tool_result", "content": content, "is_error": isError},
		}},
	})
}

func (s *session) writeTurnDuration(started time.Time) error {
	return s.writeLine(map[string]any{
		"type": "system", "subtype": "turn_duration",
		"durationMs": time.Since(started).Milliseconds(), "isMeta": false,
	})
}

func envMS(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}
