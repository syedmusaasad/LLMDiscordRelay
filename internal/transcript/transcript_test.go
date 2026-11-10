package transcript_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
)

const fixtureID = "11111111-2222-4333-8444-555555555555"

func fixtureStore() transcript.Store {
	return transcript.Store{Root: filepath.Join("..", "..", "testdata", "claude")}
}

func fixtureEvents(t *testing.T) []transcript.Event {
	t.Helper()
	p, err := fixtureStore().Path(fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := transcript.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestReadAllFixtureKinds(t *testing.T) {
	var kinds []transcript.Kind
	for _, ev := range fixtureEvents(t) {
		kinds = append(kinds, ev.Kind)
	}
	want := []transcript.Kind{
		transcript.KindUserPrompt,    // Create hello.txt ...
		transcript.KindToolCall,      // Bash
		transcript.KindToolResult,    // ok
		transcript.KindAssistantText, // reply
		transcript.KindTurnEnd,       // turn_duration
		transcript.KindUserPrompt,    // rm hello.txt
		transcript.KindToolCall,      // Bash
		transcript.KindToolResult,    // rejected (is_error)
		transcript.KindTurnEnd,       // interrupt
		transcript.KindTurnEnd,       // turn_duration after the interrupt
		transcript.KindUserPrompt,    // AskUserQuestion prompt
	}
	if len(kinds) < len(want) {
		t.Fatalf("kinds = %v, want prefix %v", kinds, want)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("kind[%d] = %v, want %v (all: %v)", i, kinds[i], k, kinds)
		}
	}
}

func TestFixtureDetails(t *testing.T) {
	evs := fixtureEvents(t)
	if evs[0].Text != "Create hello.txt containing hi, then run: ls -la" {
		t.Fatalf("prompt text = %q", evs[0].Text)
	}
	call, res := evs[1], evs[2]
	if call.ToolName != "Bash" || call.ToolID != "toolu_01AnBXLjfaCfcncKYUbr5xof" || call.MessageID != "msg_011CgpV7DTJVYh8HnJUgJaSY" {
		t.Fatalf("tool call = %+v", call)
	}
	if call.Model != "claude-haiku-5-5" {
		t.Fatalf("model = %q", call.Model)
	}
	if res.ToolID != call.ToolID || res.IsError {
		t.Fatalf("tool result = %+v", res)
	}
	if !strings.HasPrefix(evs[3].Text, "I created `hello.txt`") {
		t.Fatalf("assistant text = %q", evs[3].Text)
	}
	if evs[4].Interrupted {
		t.Fatal("turn_duration must not be marked interrupted")
	}
	if !evs[7].IsError {
		t.Fatal("rejected tool result must carry IsError")
	}
	if !evs[8].Interrupted || evs[8].Kind != transcript.KindTurnEnd {
		t.Fatalf("interrupt event = %+v", evs[8])
	}
	if evs[0].Time.IsZero() {
		t.Fatal("timestamp not parsed")
	}
}

func TestParseLineSkips(t *testing.T) {
	cases := map[string]string{
		"sidechain user":   `{"type":"user","isSidechain":true,"message":{"role":"user","content":"sub"}}`,
		"sidechain asst":   `{"type":"assistant","isSidechain":true,"message":{"id":"m","content":[{"type":"text","text":"x"}]}}`,
		"meta":             `{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		"command":          `{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		"local command":    `{"type":"user","message":{"role":"user","content":"<local-command-stdout>x</local-command-stdout>"}}`,
		"system reminder":  `{"type":"user","message":{"role":"user","content":"<system-reminder>x</system-reminder>"}}`,
		"empty":            `{"type":"user","message":{"role":"user","content":"  "}}`,
		"attachment":       `{"type":"attachment","uuid":"u"}`,
		"snapshot":         `{"type":"file-history-snapshot"}`,
		"ai-title":         `{"type":"ai-title","aiTitle":"t"}`,
		"other system":     `{"type":"system","subtype":"compact_boundary"}`,
		"not json":         `{"type":"user"`,
		"thinking only":    `{"type":"assistant","message":{"id":"m","content":[{"type":"thinking","thinking":"hm"}]}}`,
		"blank asst text":  `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":" \n"}]}}`,
		"text block harns": `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"}]}}`,
	}
	for name, raw := range cases {
		if evs := transcript.ParseLine([]byte(raw)); len(evs) != 0 {
			t.Errorf("%s: got %+v, want none", name, evs)
		}
	}
}

func TestParseLineAssistantGroupsByMessageID(t *testing.T) {
	raw := `{"type":"assistant","uuid":"u1","timestamp":"2026-11-08T00:00:00.000Z","message":{"id":"msg_1","model":"m","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}}`
	evs := transcript.ParseLine([]byte(raw))
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Kind != transcript.KindAssistantText || evs[1].Kind != transcript.KindToolCall {
		t.Fatalf("kinds = %v %v", evs[0].Kind, evs[1].Kind)
	}
	if evs[0].MessageID != "msg_1" || evs[1].MessageID != "msg_1" {
		t.Fatal("blocks of one API response must share the message id")
	}
}

func TestParseLineUserBlocksAndInterrupt(t *testing.T) {
	// Plain text block list (pasted prompt) is a prompt.
	evs := transcript.ParseLine([]byte(`{"type":"user","uuid":"u","message":{"role":"user","content":[{"type":"text","text":"hi there"}]}}`))
	if len(evs) != 1 || evs[0].Kind != transcript.KindUserPrompt || evs[0].Text != "hi there" {
		t.Fatalf("events = %+v", evs)
	}
	// String interrupt also ends the turn.
	evs = transcript.ParseLine([]byte(`{"type":"user","uuid":"u","message":{"role":"user","content":"[Request interrupted by user]"}}`))
	if len(evs) != 1 || evs[0].Kind != transcript.KindTurnEnd || !evs[0].Interrupted {
		t.Fatalf("events = %+v", evs)
	}
	evs = transcript.ParseLine([]byte(`{"type":"system","subtype":"turn_duration","uuid":"u"}`))
	if len(evs) != 1 || evs[0].Kind != transcript.KindTurnEnd || evs[0].Interrupted {
		t.Fatalf("events = %+v", evs)
	}
}

func TestStorePathRefusesNonUUID(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proj", "secret.jsonl"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := transcript.Store{Root: root}
	for _, id := range []string{"", "secret", "*", "../proj/secret", "sess-abc", fixtureID + "/../x", "1111111-2222-4333-8444-555555555555"} {
		if _, err := s.Path(id); err == nil {
			t.Errorf("Path(%q) succeeded, want refusal", id)
		}
	}
	if s.HasPrompt("*", "x") {
		t.Error("HasPrompt must refuse glob ids")
	}
	if _, err := fixtureStore().Path(fixtureID); err != nil {
		t.Errorf("fixture id must resolve: %v", err)
	}
	if transcript.ValidSessionID("not-a-uuid") || !transcript.ValidSessionID(fixtureID) {
		t.Error("ValidSessionID wrong")
	}
}

func TestHasPrompt(t *testing.T) {
	s := fixtureStore()
	if !s.HasPrompt(fixtureID, "rm hello.txt") {
		t.Error("expected prompt to be found")
	}
	if s.HasPrompt(fixtureID, "never typed this") {
		t.Error("unexpected prompt match")
	}
	if s.HasPrompt("99999999-2222-4333-8444-555555555555", "rm") {
		t.Error("unknown session must not match")
	}
}

func TestInfoAndList(t *testing.T) {
	s := fixtureStore()
	info, err := s.Info(fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != fixtureID || info.Title != "Hello.txt file creation" || info.Model != "claude-haiku-5-5" || info.CWD != "/work/demo" {
		t.Fatalf("info = %+v", info)
	}
	if info.Updated.IsZero() {
		t.Fatal("updated time missing")
	}
	list := s.List(10)
	if len(list) != 1 || list[0].ID != fixtureID {
		t.Fatalf("list = %+v", list)
	}
	if _, err := s.Info("99999999-2222-4333-8444-555555555555"); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestListOrderingLimitAndFallbackTitle(t *testing.T) {
	root := t.TempDir()
	write := func(slug, id, body string, age time.Duration) {
		dir := filepath.Join(root, slug)
		_ = os.MkdirAll(dir, 0755)
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		_ = os.Chtimes(p, at, at)
	}
	old := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	newer := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	write("-a", old, `{"type":"user","uuid":"1","message":{"role":"user","content":"first prompt becomes the title\nsecond line"}}`+"\n", 2*time.Hour)
	write("-b", newer, `{"type":"user","uuid":"2","message":{"role":"user","content":"other"}}`+"\n", time.Minute)
	write("-b", "notauuid", `{}`+"\n", 0)
	s := transcript.Store{Root: root}
	list := s.List(10)
	if len(list) != 2 || list[0].ID != newer || list[1].ID != old {
		t.Fatalf("list = %+v", list)
	}
	if list[1].Title != "first prompt becomes the title" {
		t.Fatalf("fallback title = %q", list[1].Title)
	}
	if got := s.List(1); len(got) != 1 || got[0].ID != newer {
		t.Fatalf("limited list = %+v", got)
	}
}

func TestReadNewHoldsPartialLine(t *testing.T) {
	root := t.TempDir()
	id := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	dir := filepath.Join(root, "-p")
	_ = os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, id+".jsonl")
	line1 := `{"type":"user","uuid":"u1","message":{"role":"user","content":"one"}}` + "\n"
	line2 := `{"type":"user","uuid":"u2","message":{"role":"user","content":"two"}}` + "\n"
	// First write: one full line and half of the second.
	if err := os.WriteFile(path, []byte(line1+line2[:20]), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := transcript.NewTailer(transcript.Store{Root: root}, id, 10*time.Millisecond).Tail(ctx)

	expect := func(text string) {
		t.Helper()
		select {
		case ev := <-ch:
			if ev.Text != text {
				t.Fatalf("event text = %q, want %q", ev.Text, text)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %q", text)
		}
	}
	expect("one")
	select {
	case ev := <-ch:
		t.Fatalf("partial line produced event %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line2[20:]); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	expect("two")
}

func TestTailerWaitsForFileAndStartOffset(t *testing.T) {
	root := t.TempDir()
	id := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	path := filepath.Join(root, "-p", id+".jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tl := transcript.NewTailer(transcript.Store{Root: root}, id, 10*time.Millisecond)
	ch := tl.Tail(ctx)
	time.Sleep(60 * time.Millisecond) // file does not exist yet: no events, no panic
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	line1 := `{"type":"user","uuid":"u1","message":{"role":"user","content":"early"}}` + "\n"
	if err := os.WriteFile(path, []byte(line1), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Text != "early" {
			t.Fatalf("text = %q", ev.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tailer never saw the late-created file")
	}
	cancel()

	// A tailer started at the end of the file only reports new lines.
	store := transcript.Store{Root: root}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	tl2 := transcript.NewTailer(store, id, 10*time.Millisecond)
	tl2.StartOffset = store.Size(id)
	ch2 := tl2.Tail(ctx2)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"user","uuid":"u2","message":{"role":"user","content":"late"}}` + "\n")
	_ = f.Close()
	select {
	case ev := <-ch2:
		if ev.Text != "late" {
			t.Fatalf("text = %q, want only the new line", ev.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event from offset tailer")
	}
}

func TestWaitForTranscriptHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := transcript.Store{Root: t.TempDir()}.WaitForTranscript(ctx, fixtureID, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("RELAY_CLAUDE_PROJECTS", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	if r, _ := transcript.DefaultRoot(); r != "/cfg/projects" {
		t.Fatalf("root = %q", r)
	}
	t.Setenv("RELAY_CLAUDE_PROJECTS", "/explicit")
	if r, _ := transcript.DefaultRoot(); r != "/explicit" {
		t.Fatalf("root = %q", r)
	}
	t.Setenv("RELAY_CLAUDE_PROJECTS", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/home/x")
	if r, _ := transcript.DefaultRoot(); r != "/home/x/.claude/projects" {
		t.Fatalf("root = %q", r)
	}
}
