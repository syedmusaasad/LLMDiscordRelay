package inject

import (
	"regexp"
	"strings"
	"unicode"
)

// Pane parsers for Claude Code 2.1.319. They work on text from
// `tmux capture-pane -p`; none of them touch tmux themselves.

const (
	// Signature of the refusal message `claude --resume <unknown id>` prints
	// before exiting 1.
	ResumeRefusedSignature = "No conversation found with session ID"

	busyFooter    = "esc to interrupt"
	trustHeading  = "Quick safety check"
	trustYes      = "Yes, I trust this folder"
	cancelFooter  = "Esc to cancel"
	pickerFooter  = "Enter to select"
	pickerHeader  = "☐"
	typeSomething = "Type something"
	chatAboutThis = "Chat about this"
)

// permHeadingRe matches the question above the options: "Do you want to
// proceed?" for shell commands, "Do you want to make this edit to X?" etc. for
// file tools.
var permHeadingRe = regexp.MustCompile(`^\s*Do you want to .*\?\s*$`)

var numberedOptionRe = regexp.MustCompile(`^\s*(?:❯\s*)?(\d+)\.\s+(.*\S)\s*$`)

func isRule(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "────")
}

func isDashedRule(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "╌╌╌")
}

// IsResumeRefused reports whether the pane shows the real CLI's refusal for an
// unknown --resume id.
func IsResumeRefused(pane string) bool {
	pane = normalize(pane)
	return strings.Contains(pane, ResumeRefusedSignature)
}

// IsTrustDialog reports whether the pane shows the first-run folder-trust
// dialog ("❯ No, exit" selected, "Yes, I trust this folder" below).
func IsTrustDialog(pane string) bool {
	pane = normalize(pane)
	return strings.Contains(pane, trustHeading) && strings.Contains(pane, trustYes)
}

// IsPermissionPrompt reports whether the pane shows a tool permission prompt.
func IsPermissionPrompt(pane string) bool {
	pane = normalize(pane)
	if !strings.Contains(pane, cancelFooter) || !strings.Contains(pane, "1. Yes") {
		return false
	}
	for _, l := range strings.Split(pane, "\n") {
		if permHeadingRe.MatchString(l) {
			return true
		}
	}
	return false
}

// PermissionSignature identifies one permission prompt for de-duplication: the
// command/action text shown above "Do you want to proceed?", back to the
// nearest full-width rule, without the dashed ╌╌╌ rules or blank lines.
// It is empty when the pane has no permission prompt.
func PermissionSignature(pane string) string {
	pane = normalize(pane)
	if !IsPermissionPrompt(pane) {
		return ""
	}
	lines := strings.Split(pane, "\n")
	heading := -1
	for i, l := range lines {
		if permHeadingRe.MatchString(l) {
			heading = i // the last prompt on screen
		}
	}
	if heading < 0 {
		return ""
	}
	var block []string
	for i := heading - 1; i >= 0; i-- {
		if isRule(lines[i]) {
			break
		}
		if isDashedRule(lines[i]) || strings.TrimSpace(lines[i]) == "" {
			continue
		}
		block = append([]string{strings.TrimSpace(lines[i])}, block...)
	}
	return strings.Join(block, "\n")
}

// Question is a parsed AskUserQuestion picker.
type Question struct {
	Header  string
	Prompt  string
	Options []string // only the real options; never "Type something." / "Chat about this"
}

// ParseQuestion parses the AskUserQuestion picker: "☐ <header>", the question
// line, numbered options (descriptions indented beneath are ignored) and the
// "Enter to select · ↑/↓ to navigate · Esc to cancel" footer.
func ParseQuestion(pane string) (Question, bool) {
	pane = normalize(pane)
	if !strings.Contains(pane, pickerFooter) || !strings.Contains(pane, cancelFooter) {
		return Question{}, false
	}
	lines := strings.Split(pane, "\n")
	header := -1
	for i, l := range lines {
		if strings.Contains(l, pickerHeader) {
			header = i
		}
	}
	if header < 0 {
		return Question{}, false
	}
	q := Question{Header: strings.TrimSpace(strings.SplitN(lines[header], pickerHeader, 2)[1])}
	for _, l := range lines[header+1:] {
		t := strings.TrimSpace(l)
		if t == "" || isRule(l) {
			continue
		}
		if strings.Contains(t, pickerFooter) {
			break
		}
		if m := numberedOptionRe.FindStringSubmatch(l); m != nil {
			label := strings.TrimSpace(m[2])
			if strings.HasPrefix(label, typeSomething) || strings.HasPrefix(label, chatAboutThis) {
				continue
			}
			q.Options = append(q.Options, label)
			continue
		}
		if q.Prompt == "" && len(q.Options) == 0 {
			q.Prompt = t
		}
	}
	if q.Prompt == "" || len(q.Options) == 0 {
		return Question{}, false
	}
	return q, true
}

// PaneReady reports whether captured pane text shows Claude Code idle and
// accepting a prompt: an input line starting with "❯ " between two "────"
// rules, and a footer that does not say "esc to interrupt". Dialogs that draw
// "❯ N." selection rows (trust, permission, picker) are never ready.
func PaneReady(pane string) bool {
	pane = normalize(pane)
	if IsTrustDialog(pane) || IsPermissionPrompt(pane) {
		return false
	}
	if _, picker := ParseQuestion(pane); picker {
		return false
	}
	lines := strings.Split(pane, "\n")
	lower, upper := -1, -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !isRule(lines[i]) {
			continue
		}
		if lower < 0 {
			lower = i
			continue
		}
		upper = i
		break
	}
	if upper < 0 || lower-upper < 2 {
		return false
	}
	input := strings.TrimLeftFunc(lines[upper+1], unicode.IsSpace)
	if !strings.HasPrefix(input, "❯") || numberedOptionRe.MatchString(lines[upper+1]) {
		return false
	}
	return !strings.Contains(strings.Join(lines[lower+1:], "\n"), busyFooter)
}

// normalize maps the non-breaking space Claude Code draws after "❯" to a plain
// space so the parsers can use ordinary whitespace matching.
func normalize(pane string) string {
	return strings.ReplaceAll(pane, "\u00a0", " ")
}
