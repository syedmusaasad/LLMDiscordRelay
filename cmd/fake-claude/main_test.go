package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/tmuxctl"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

func TestRenderersMatchTheRelaysParsers(t *testing.T) {
	idle := renderIdle("/work/demo", []string{"> hi", "● hello"}, "", false)
	if !inject.PaneReady(idle) {
		t.Fatalf("idle frame must be ready:\n%s", idle)
	}
	if inject.PaneReady(renderIdle("/work/demo", nil, "", true)) {
		t.Fatal("busy frame must not be ready")
	}
	trust := renderTrust("/work/demo", 0)
	if !inject.IsTrustDialog(trust) || inject.PaneReady(trust) {
		t.Fatalf("trust frame:\n%s", trust)
	}
	perm := renderPermission("/work/demo", nil, "ls -la", "List files", 0)
	if !inject.IsPermissionPrompt(perm) || inject.PaneReady(perm) {
		t.Fatalf("permission frame:\n%s", perm)
	}
	if sig := inject.PermissionSignature(perm); sig != "Bash command\nls -la\nList files" {
		t.Fatalf("signature = %q", sig)
	}
	q, ok := inject.ParseQuestion(renderQuestion("/work/demo", nil, "Color", "Which color do you prefer?", []string{"Red", "Green"}, 0))
	if !ok || strings.Join(q.Options, ",") != "Red,Green" || q.Prompt != "Which color do you prefer?" {
		t.Fatalf("question = %+v ok=%v", q, ok)
	}
}

func TestTranscriptLinesAreReadableByTheRelay(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	s := &session{id: id, cwd: "/work/demo", root: root}
	started := time.Now()
	if err := s.writeUser("hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.writeAssistantTool("toolu_1", "Bash", map[string]any{"command": "ls"}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeToolResult("toolu_1", rejectedText, true); err != nil {
		t.Fatal(err)
	}
	if err := s.interrupted(started, "for tool use"); err != nil {
		t.Fatal(err)
	}
	p, err := transcript.Store{Root: root}.Path(id)
	if err != nil || !strings.Contains(p, "-work-demo") {
		t.Fatalf("path = %q, %v", p, err)
	}
	evs, err := transcript.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []transcript.Kind
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []transcript.Kind{transcript.KindUserPrompt, transcript.KindToolCall, transcript.KindToolResult, transcript.KindTurnEnd, transcript.KindTurnEnd}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if !evs[2].IsError || !evs[3].Interrupted {
		t.Fatalf("events = %+v", evs)
	}
}

// fakeInTmux builds fake-claude and runs it in a tmux pane with args and env.
func fakeInTmux(t *testing.T, name, root string, env map[string]string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	bin := filepath.Join(t.TempDir(), "fake-claude")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	work := t.TempDir()
	cmd := "cd '" + work + "' && env RELAY_CLAUDE_PROJECTS='" + root + "'"
	for k, v := range env {
		cmd += " " + k + "='" + v + "'"
	}
	cmd += " '" + bin + "'"
	for _, a := range args {
		cmd += " '" + a + "'"
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", name).Run() })
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "120", "-y", "40", cmd).CombinedOutput(); err != nil {
		t.Fatalf("tmux: %v\n%s", err, out)
	}
	return name + ":0.0"
}

func capture(t *testing.T, target string) string {
	t.Helper()
	out, err := exec.Command("tmux", "capture-pane", "-t", target, "-p").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFakeClaudeSessionInTmux(t *testing.T) {
	root := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	target := fakeInTmux(t, "llm-fake-session", root, nil, "--session-id", id)
	store := transcript.Store{Root: root}

	if !inject.WaitReady(target) {
		t.Fatalf("never ready:\n%s", capture(t, target))
	}
	if _, err := store.Path(id); err == nil {
		t.Fatal("the transcript must not exist before the first prompt")
	}
	if err := inject.SendText(target, "ping"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first transcript", func() bool { return store.HasPrompt(id, "ping") })
	waitFor(t, "ready again", func() bool { return inject.PaneReady(capture(t, target)) })

	// A 3-line prompt must arrive as ONE prompt through bracketed paste.
	multi := "line one\nline two\nline three"
	if err := inject.SendText(target, multi); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "multi-line prompt", func() bool { return store.HasPrompt(id, multi) })
	waitFor(t, "second turn end", func() bool {
		p, _ := store.Path(id)
		evs, _ := transcript.ReadAll(p)
		n := 0
		for _, e := range evs {
			if e.Kind == transcript.KindTurnEnd {
				n++
			}
		}
		return n == 2
	})
	p, _ := store.Path(id)
	evs, _ := transcript.ReadAll(p)
	prompts := 0
	for _, e := range evs {
		if e.Kind == transcript.KindUserPrompt {
			prompts++
		}
	}
	if prompts != 2 {
		t.Fatalf("prompts = %d, want 2 (multi-line must not split)", prompts)
	}
}

func TestFakeClaudeResumeRefusal(t *testing.T) {
	root := t.TempDir()
	target := fakeInTmux(t, "llm-fake-refuse", root, map[string]string{"FAKE_CLAUDE_REFUSE_LINGER_MS": "3000"}, "--resume", "99999999-2222-4333-8444-555555555555")
	waitFor(t, "refusal message", func() bool { return inject.IsResumeRefused(capture(t, target)) })
}

func TestFakeClaudeTrustAndPermission(t *testing.T) {
	root := t.TempDir()
	id := "22222222-2222-4333-8444-555555555555"
	target := fakeInTmux(t, "llm-fake-trust", root, map[string]string{
		"FAKE_CLAUDE_UNTRUSTED": "1", "FAKE_CLAUDE_PERMISSION_PROMPT": "1",
	}, "--session-id", id)
	store := transcript.Store{Root: root}
	waitFor(t, "trust dialog", func() bool { return inject.IsTrustDialog(capture(t, target)) })
	if err := tmuxctl.SendRecipe(target, tmuxctl.TrustAcceptRecipe); err != nil {
		t.Fatal(err)
	}
	if !inject.WaitReady(target) {
		t.Fatalf("not ready after trusting:\n%s", capture(t, target))
	}
	if err := inject.SendText(target, "run ls"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "permission prompt", func() bool { return inject.IsPermissionPrompt(capture(t, target)) })
	if err := tmuxctl.SendRecipe(target, tmuxctl.ApprovalApproveRecipe); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn end", func() bool {
		p, err := store.Path(id)
		if err != nil {
			return false
		}
		evs, _ := transcript.ReadAll(p)
		return len(evs) > 0 && evs[len(evs)-1].Kind == transcript.KindTurnEnd
	})
}

func TestFakeClaudePermissionDenyAndQuestion(t *testing.T) {
	root := t.TempDir()
	id := "33333333-2222-4333-8444-555555555555"
	target := fakeInTmux(t, "llm-fake-deny", root, map[string]string{
		"FAKE_CLAUDE_PERMISSION_PROMPT": "1",
	}, "--session-id", id)
	store := transcript.Store{Root: root}
	if !inject.WaitReady(target) {
		t.Fatal("not ready")
	}
	_ = inject.SendText(target, "do it")
	waitFor(t, "permission prompt", func() bool { return inject.IsPermissionPrompt(capture(t, target)) })
	if err := tmuxctl.SendRecipe(target, tmuxctl.ApprovalDenyRecipe); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "interrupt", func() bool {
		p, err := store.Path(id)
		if err != nil {
			return false
		}
		evs, _ := transcript.ReadAll(p)
		for _, e := range evs {
			if e.Kind == transcript.KindTurnEnd && e.Interrupted {
				return true
			}
		}
		return false
	})

	qid := "44444444-2222-4333-8444-555555555555"
	qt := fakeInTmux(t, "llm-fake-question", root, map[string]string{"FAKE_CLAUDE_QUESTION_PICKER": "1"}, "--session-id", qid)
	if !inject.WaitReady(qt) {
		t.Fatal("not ready")
	}
	_ = inject.SendText(qt, "ask me")
	waitFor(t, "picker", func() bool { _, ok := inject.ParseQuestion(capture(t, qt)); return ok })
	if err := tmuxctl.SelectQuestion(qt, 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "answer recorded", func() bool {
		p, err := store.Path(qid)
		if err != nil {
			return false
		}
		data, _ := os.ReadFile(p)
		return strings.Contains(string(data), "User has answered your questions: Green")
	})
}
