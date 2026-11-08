package inject

import (
	"strings"
	"testing"
)

// Pane texts for Claude Code 2.1.319, as described by the verified behaviour
// notes (docs/BINDING-PROTOCOL.md).
const rule = "────────────────────────────────────────────────────────────────────────"

const readyPane = ` ▐▛███▜▌   Claude Code v2.1.319
▝▜█████▛▘  Haiku 5.5 · Claude Max
  ▘▘ ▝▝    /work/demo

` + rule + `
❯ ` + `
` + rule + `
  ? for shortcuts
`

const busyPane = `
● Running…

` + rule + `
❯ ` + `
` + rule + `
  esc to interrupt
`

const trustPane = ` Accessing workspace:

 /work/demo

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team).

 ❯ 1. No, exit
   2. Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

// The trust dialog as the real TUI draws it: the selected row has no number.
const trustPaneUnnumbered = `Quick safety check: Is this a project you created or one you trust?

❯ No, exit
  Yes, I trust this folder
`

const permissionPane = ` Bash command

   rm hello.txt && ls -la
   Remove hello.txt and list directory contents

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to demo/ from this project
   3. No

 Esc to cancel · Tab to amend
`

const editPermissionPane = rule + `
 Edit file
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 hello.txt
 + hi
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to make this edit to hello.txt?
`

const questionPane = rule + `
☐ Color

Which color do you prefer?

❯ 1. Red
     The color red
  2. Green
     The color green
  3. Type something.
` + rule + `
  Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

func TestShouldAcceptAuthor_EmptyIDRejectsAll(t *testing.T) {
	for _, author := range []string{"", "123", "user-allowed-001"} {
		for _, bot := range []bool{false, true} {
			if ShouldAcceptAuthor(author, bot, "") {
				t.Fatalf("ShouldAcceptAuthor(%q, bot=%v, \"\") = true, want false", author, bot)
			}
		}
	}
	if !ShouldAcceptAuthor("u1", false, "u1") {
		t.Fatal("configured user must still be accepted")
	}
	if ShouldAcceptAuthor("u2", false, "u1") {
		t.Fatal("other user must be rejected")
	}
}

func TestPaneReady(t *testing.T) {
	if !PaneReady(readyPane) {
		t.Fatal("idle pane must be ready")
	}
	// Claude Code draws a non-breaking space after the prompt glyph.
	if !PaneReady(strings.ReplaceAll(readyPane, "❯ ", "❯\u00a0")) {
		t.Fatal("idle pane with NBSP prompt must be ready")
	}
	if !PaneReady(strings.Replace(readyPane, "❯ ", "❯ half-typed text", 1)) {
		t.Fatal("pane with typed input is still ready")
	}
	if PaneReady(busyPane) {
		t.Fatal("busy pane (esc to interrupt) must not be ready")
	}
	for name, pane := range map[string]string{
		"trust":      trustPane,
		"permission": permissionPane + "\n" + rule + "\n❯ \n" + rule + "\n",
		"question":   questionPane,
		"empty":      "",
		"booting":    "loading...\n",
		"one rule":   rule + "\n❯ \n",
		"no prompt":  rule + "\n  plain\n" + rule + "\n  ? for shortcuts\n",
	} {
		if PaneReady(pane) {
			t.Errorf("%s pane must not be ready", name)
		}
	}
}

func TestTrustDialog(t *testing.T) {
	if !IsTrustDialog(trustPane) || !IsTrustDialog(trustPaneUnnumbered) {
		t.Fatal("trust dialog not recognised")
	}
	if IsTrustDialog(readyPane) || IsTrustDialog(permissionPane) {
		t.Fatal("false positive trust dialog")
	}
}

func TestPermissionPrompt(t *testing.T) {
	if !IsPermissionPrompt(permissionPane) || !IsPermissionPrompt(editPermissionPane+" ❯ 1. Yes\n 3. No\n Esc to cancel") {
		t.Fatal("permission prompt not recognised")
	}
	if IsPermissionPrompt(readyPane) || IsPermissionPrompt(questionPane) || IsPermissionPrompt(trustPane) {
		t.Fatal("false positive permission prompt")
	}
	sig := PermissionSignature(permissionPane)
	want := "Bash command\nrm hello.txt && ls -la\nRemove hello.txt and list directory contents"
	if sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
	// Different command, different signature; same command redrawn, same signature.
	other := strings.ReplaceAll(permissionPane, "rm hello.txt", "ls /tmp")
	if PermissionSignature(other) == sig {
		t.Fatal("signature must change with the command")
	}
	if PermissionSignature(permissionPane) != sig {
		t.Fatal("signature must be stable")
	}
	editFull := editPermissionPane + " ❯ 1. Yes\n   2. Yes, allow all edits\n   3. No\n\n Esc to cancel\n"
	if got := PermissionSignature(editFull); strings.Contains(got, "╌") || !strings.Contains(got, "hello.txt") {
		t.Fatalf("edit signature = %q", got)
	}
	if PermissionSignature(readyPane) != "" {
		t.Fatal("no prompt, no signature")
	}
}

func TestParseQuestion(t *testing.T) {
	q, ok := ParseQuestion(questionPane)
	if !ok {
		t.Fatal("picker not recognised")
	}
	if q.Header != "Color" || q.Prompt != "Which color do you prefer?" {
		t.Fatalf("question = %+v", q)
	}
	if strings.Join(q.Options, "|") != "Red|Green" {
		t.Fatalf("options = %v; must exclude Type something. and Chat about this", q.Options)
	}
	for name, pane := range map[string]string{"ready": readyPane, "permission": permissionPane, "trust": trustPane} {
		if _, ok := ParseQuestion(pane); ok {
			t.Errorf("%s pane misparsed as a question", name)
		}
	}
}

func TestResumeRefused(t *testing.T) {
	if !IsResumeRefused("No conversation found with session ID: 11111111-2222-4333-8444-555555555555\n") {
		t.Fatal("refusal not recognised")
	}
	if IsResumeRefused(readyPane) {
		t.Fatal("false positive refusal")
	}
}
