// Package transcript reads Claude Code session transcripts.
//
// Claude Code appends one JSON object per line to
// <projects>/<project-slug>/<session-id>.jsonl, where <projects> is
// $CLAUDE_CONFIG_DIR/projects (default ~/.claude/projects). The file is created
// when the session's first prompt is submitted, not at launch. The relay only
// reads these files; it never writes them.
//
// Schema reference: docs/TRANSCRIPT-SCHEMA.md
// Verified against Claude Code 2.1.319.
package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind classifies a transcript event.
type Kind int

const (
	KindUnknown       Kind = iota
	KindUserPrompt         // a prompt the user submitted (typed, pasted or relayed)
	KindAssistantText      // a text block of an assistant message
	KindToolCall           // a tool_use block of an assistant message
	KindToolResult         // the result of a tool call (carries IsError)
	KindTurnEnd            // the turn finished (system/turn_duration, or an interrupt)
)

func (k Kind) String() string {
	switch k {
	case KindUserPrompt:
		return "user-prompt"
	case KindAssistantText:
		return "assistant-text"
	case KindToolCall:
		return "tool-call"
	case KindToolResult:
		return "tool-result"
	case KindTurnEnd:
		return "turn-end"
	}
	return "unknown"
}

// Event is one mirrorable fact from a transcript line. A single line can yield
// several events (an assistant line may carry text and tool_use blocks).
type Event struct {
	Kind      Kind
	MessageID string // assistant message id (groups blocks of one API response); user line uuid otherwise
	UUID      string // the transcript line's uuid
	Time      time.Time
	Text      string // KindUserPrompt, KindAssistantText
	ToolName  string // KindToolCall
	ToolID    string // KindToolCall, KindToolResult
	IsError   bool   // KindToolResult
	// Interrupted is set on a KindTurnEnd that came from an interrupt (Esc, or
	// "No" on a permission prompt) rather than a completed turn.
	Interrupted bool
	Model       string // assistant lines: the model that produced them
}

// line is the subset of a transcript line the relay reads.
type line struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	UUID        string          `json:"uuid"`
	Timestamp   string          `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	CWD         string          `json:"cwd"`
	AITitle     string          `json:"aiTitle"`
	Message     json.RawMessage `json:"message"`
}

type message struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

// interruptPrefix marks the user line Claude Code writes when a turn is
// interrupted (Esc, or "No" on a permission prompt). No turn_duration follows
// such a turn, so the interrupt itself ends it.
const interruptPrefix = "[Request interrupted by user"

// isHarnessText reports user text that Claude Code injected rather than the
// user typed: slash-command records, local command output and reminders.
func isHarnessText(s string) bool {
	t := strings.TrimSpace(s)
	for _, p := range []string{"<command-", "<local-command-", "<system-reminder>", "<bash-", "<user-memory-input>"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// ParseLine decodes one transcript line into zero or more events. Lines that
// carry nothing mirrorable (attachments, snapshots, request records, titles,
// sidechain/subagent traffic) yield none.
func ParseLine(raw []byte) []Event {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.IsSidechain {
		return nil
	}
	ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	switch l.Type {
	case "system":
		if l.Subtype == "turn_duration" {
			return []Event{{Kind: KindTurnEnd, UUID: l.UUID, MessageID: l.UUID, Time: ts}}
		}
		return nil
	case "user":
		if l.IsMeta {
			return nil
		}
		var m message
		if json.Unmarshal(l.Message, &m) != nil {
			return nil
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return userText(l, ts, s)
		}
		var blocks []block
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil
		}
		var out []Event
		var texts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				out = append(out, Event{Kind: KindToolResult, UUID: l.UUID, MessageID: l.UUID, Time: ts,
					ToolID: b.ToolUseID, IsError: b.IsError})
			case "text":
				texts = append(texts, b.Text)
			}
		}
		if len(out) == 0 && len(texts) > 0 {
			return userText(l, ts, strings.Join(texts, "\n"))
		}
		return out
	case "assistant":
		var m message
		if json.Unmarshal(l.Message, &m) != nil {
			return nil
		}
		var blocks []block
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil
		}
		var out []Event
		for _, b := range blocks {
			ev := Event{UUID: l.UUID, MessageID: m.ID, Time: ts, Model: m.Model}
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				ev.Kind, ev.Text = KindAssistantText, b.Text
			case "tool_use":
				ev.Kind, ev.ToolName, ev.ToolID = KindToolCall, b.Name, b.ID
			default:
				continue // thinking, redacted_thinking, ...
			}
			out = append(out, ev)
		}
		return out
	}
	return nil
}

func userText(l line, ts time.Time, s string) []Event {
	if strings.HasPrefix(strings.TrimSpace(s), interruptPrefix) {
		return []Event{{Kind: KindTurnEnd, UUID: l.UUID, MessageID: l.UUID, Time: ts, Interrupted: true}}
	}
	if isHarnessText(s) || strings.TrimSpace(s) == "" {
		return nil
	}
	return []Event{{Kind: KindUserPrompt, UUID: l.UUID, MessageID: l.UUID, Time: ts, Text: s}}
}

// ─── Store: locating transcripts ────────────────────────────────────────────

// Store locates transcripts under a Claude Code projects directory.
type Store struct {
	Root string
}

// DefaultRoot returns the projects directory: $RELAY_CLAUDE_PROJECTS, else
// $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects.
func DefaultRoot() (string, error) {
	if v := os.Getenv("RELAY_CLAUDE_PROJECTS"); v != "" {
		return v, nil
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

var sessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidSessionID reports whether id has the UUID shape Claude Code requires
// for --session-id and --resume. Anything else is refused before it can reach
// a glob or a command line.
func ValidSessionID(id string) bool { return sessionIDRe.MatchString(id) }

// ErrNotFound is returned when no transcript exists for a session id.
var ErrNotFound = errors.New("transcript: not found")

// Path returns the transcript file for sessionID. Subagent transcripts live
// one directory deeper and are never matched.
func (s Store) Path(sessionID string) (string, error) {
	if !ValidSessionID(sessionID) {
		return "", ErrNotFound
	}
	matches, _ := filepath.Glob(filepath.Join(s.Root, "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return "", ErrNotFound
	}
	if len(matches) > 1 { // same id under two projects: the newest is live
		sort.Slice(matches, func(i, j int) bool { return mtime(matches[i]).After(mtime(matches[j])) })
	}
	return matches[0], nil
}

func mtime(p string) time.Time {
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// WaitForTranscript polls until sessionID's transcript exists or ctx ends.
func (s Store) WaitForTranscript(ctx context.Context, sessionID string, poll time.Duration) (string, error) {
	if poll == 0 {
		poll = 200 * time.Millisecond
	}
	for {
		if p, err := s.Path(sessionID); err == nil {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ─── Tailer ─────────────────────────────────────────────────────────────────

// Tailer follows one session's transcript from the beginning, emitting events
// for every complete line. A trailing partial line is held until it completes.
type Tailer struct {
	store        Store
	sessionID    string
	pollInterval time.Duration
	// StartOffset, when non-zero, skips the first StartOffset bytes of the file
	// (used to follow only new activity after a resume). It must sit on a line
	// boundary, which Store.Size guarantees for a quiescent transcript.
	StartOffset int64
}

// NewTailer creates a Tailer for sessionID.
func NewTailer(store Store, sessionID string, pollInterval time.Duration) *Tailer {
	if pollInterval == 0 {
		pollInterval = 250 * time.Millisecond
	}
	return &Tailer{store: store, sessionID: sessionID, pollInterval: pollInterval}
}

// Tail emits events until ctx is cancelled. It waits for the transcript file to
// appear, then replays it from the start and follows appends.
func (t *Tailer) Tail(ctx context.Context) <-chan Event {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		path, err := t.store.WaitForTranscript(ctx, t.sessionID, t.pollInterval)
		if err != nil {
			return
		}
		offset := t.StartOffset
		var pending []byte
		ticker := time.NewTicker(t.pollInterval)
		defer ticker.Stop()
		for {
			var events []Event
			offset, pending, events = readNew(path, offset, pending)
			for _, ev := range events {
				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ch
}

// readNew reads bytes appended since offset and parses every complete line.
func readNew(path string, offset int64, pending []byte) (int64, []byte, []Event) {
	f, err := os.Open(path)
	if err != nil {
		return offset, pending, nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < offset {
		offset, pending = 0, nil // truncated or replaced: replay
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, pending, nil
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return offset, pending, nil
	}
	offset += int64(len(data))
	buf := append(pending, data...)
	var events []Event
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		if ln := bytes.TrimSpace(buf[:i]); len(ln) > 0 {
			events = append(events, ParseLine(ln)...)
		}
		buf = buf[i+1:]
	}
	return offset, append([]byte(nil), buf...), events
}

// Size returns the current byte length of sessionID's transcript, or 0 when it
// does not exist yet.
func (s Store) Size(sessionID string) int64 {
	p, err := s.Path(sessionID)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ReadAll parses a whole transcript file.
func ReadAll(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		out = append(out, ParseLine(sc.Bytes())...)
	}
	return out, sc.Err()
}

// HasPrompt reports whether sessionID's transcript holds a user prompt that
// contains needle (used to confirm a relayed prompt was received).
func (s Store) HasPrompt(sessionID, needle string) bool {
	p, err := s.Path(sessionID)
	if err != nil {
		return false
	}
	events, _ := ReadAll(p)
	for _, ev := range events {
		if ev.Kind == KindUserPrompt && strings.Contains(ev.Text, needle) {
			return true
		}
	}
	return false
}

// ─── Session listing (for !resume) ──────────────────────────────────────────

// SessionInfo summarises one transcript.
type SessionInfo struct {
	ID      string
	Path    string
	CWD     string
	Title   string
	Model   string
	Updated time.Time
}

// Info summarises sessionID's transcript.
func (s Store) Info(sessionID string) (SessionInfo, error) {
	p, err := s.Path(sessionID)
	if err != nil {
		return SessionInfo{}, err
	}
	return summarise(p), nil
}

// List returns up to limit top-level sessions, most recently updated first.
func (s Store) List(limit int) []SessionInfo {
	matches, _ := filepath.Glob(filepath.Join(s.Root, "*", "*.jsonl"))
	var out []SessionInfo
	for _, p := range matches {
		if !ValidSessionID(strings.TrimSuffix(filepath.Base(p), ".jsonl")) {
			continue
		}
		out = append(out, SessionInfo{ID: strings.TrimSuffix(filepath.Base(p), ".jsonl"), Path: p, Updated: mtime(p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = summarise(out[i].Path)
	}
	return out
}

func summarise(path string) SessionInfo {
	info := SessionInfo{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Updated: mtime(path)}
	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	firstPrompt := ""
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if info.CWD == "" && l.CWD != "" {
			info.CWD = l.CWD
		}
		if l.Type == "ai-title" && l.AITitle != "" {
			info.Title = l.AITitle
		}
		for _, ev := range ParseLine(sc.Bytes()) {
			if ev.Kind == KindUserPrompt && firstPrompt == "" {
				firstPrompt = ev.Text
			}
			if ev.Model != "" {
				info.Model = ev.Model
			}
		}
	}
	if info.Title == "" {
		info.Title = firstLine(firstPrompt, 60)
	}
	return info
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
