package mirror_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/mirror"
)

// ── TeardownPane tests ────────────────────────────────────────────────────────

// TestTeardownPane_DoesNotKillOtherWindow creates a two-window tmux session,
// runs TeardownPane on the first window, and asserts the session and the second
// window both survive (i.e., kill-session is never called).
func TestTeardownPane_DoesNotKillOtherWindow(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	sessionName := "test-teardown-twowin"
	// Ensure cleanup even if the test panics.
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })

	// Create a tmux session with two windows.
	// Window 0: a long-running process (sleep).
	if out, err := exec.Command("tmux", "new-session", "-d",
		"-s", sessionName, "-x", "80", "-y", "24",
		"sleep", "120",
	).CombinedOutput(); err != nil {
		t.Fatalf("create tmux session: %v\n%s", err, out)
	}
	// Window 1: another long-running process (sleep).
	if out, err := exec.Command("tmux", "new-window",
		"-t", sessionName,
		"sleep", "120",
	).CombinedOutput(); err != nil {
		t.Fatalf("create second window: %v\n%s", err, out)
	}

	// Verify both windows exist.
	out, err := exec.Command("tmux", "list-windows", "-t", sessionName).Output()
	if err != nil {
		t.Fatalf("list-windows before teardown: %v", err)
	}
	if !strings.Contains(string(out), "0:") || !strings.Contains(string(out), "1:") {
		t.Fatalf("expected 2 windows before teardown, got: %s", out)
	}

	// Run teardown on window 0 (target: session:0.0).
	paneTarget := sessionName + ":0.0"
	mirror.TeardownPane(paneTarget)

	// After teardown: session must still exist, window 1 must still be there.
	time.Sleep(100 * time.Millisecond)
	out2, err2 := exec.Command("tmux", "list-windows", "-t", sessionName).Output()
	if err2 != nil {
		t.Fatalf("session was killed by teardownPane (expected only the window to die): %v", err2)
	}
	if strings.Contains(string(out2), "0:") {
		t.Error("window 0 should have been killed by teardownPane")
	}
	if !strings.Contains(string(out2), "1:") {
		t.Error("window 1 should have survived teardownPane on window 0")
	}
	t.Logf("✓ teardownPane killed only window 0; session and window 1 survive")
}

// ── Chunking tests ────────────────────────────────────────────────────────────

func TestChunkText_ShortText(t *testing.T) {
	text := "Hello, world!"
	chunks := mirror.ChunkText(text, 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("expected %q, got %q", text, chunks[0])
	}
}

func TestChunkText_ExactLimit(t *testing.T) {
	text := make([]byte, 2000)
	for i := range text {
		text[i] = 'a'
	}
	chunks := mirror.ChunkText(string(text), 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestChunkText_ParagraphBreak(t *testing.T) {
	// Build text with a paragraph break within the first 2000 chars
	part1 := repeat("a", 1000)
	part2 := repeat("b", 1500)
	text := part1 + "\n\n" + part2 // total > 2000
	chunks := mirror.ChunkText(text, 2000)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk should end at the paragraph boundary
	if !endsWith(chunks[0], "\n\n") && !endsWith(chunks[0], part1) {
		t.Errorf("first chunk should break at paragraph: got %q...", chunks[0][:min(50, len(chunks[0]))]+"..")
	}
}

func TestChunkText_ReassemblesCorrectly(t *testing.T) {
	// The chunks should reassemble to the original text
	text := repeat("word ", 600) // ~3000 chars
	chunks := mirror.ChunkText(text, 2000)
	reassembled := ""
	for _, c := range chunks {
		reassembled += c
	}
	if reassembled != text {
		t.Errorf("reassembled text differs from original; len orig=%d len reassembled=%d",
			len(text), len(reassembled))
	}
}

func TestChunkText_DefaultLimit(t *testing.T) {
	text := repeat("x", 4001)
	chunks := mirror.ChunkText(text, 0) // 0 means use default 2000
	for i, c := range chunks {
		if len([]rune(c)) > 2000 {
			t.Errorf("chunk %d exceeds 2000 chars: %d", i, len([]rune(c)))
		}
	}
}

// ── Throttle tests ────────────────────────────────────────────────────────────

func TestThrottle_AllowsUpToLimit(t *testing.T) {
	th := mirror.NewThrottle(3, 5*time.Second)
	// Should allow 3 immediate calls
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		go func() {
			th.Wait("chan1")
			close(done)
		}()
		select {
		case <-done:
			// OK
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("call %d should not have blocked", i+1)
		}
	}
}

func TestThrottle_BlocksAtLimit(t *testing.T) {
	th := mirror.NewThrottle(2, 2*time.Second)
	// Exhaust the limit
	th.Wait("ch")
	th.Wait("ch")
	// Third call should block for ~2s; check it does NOT return instantly
	done := make(chan struct{})
	go func() {
		th.Wait("ch")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("third call returned immediately; expected throttle block")
	case <-time.After(100 * time.Millisecond):
		// Good – it's blocking
	}
}

func TestThrottle_BackoffDelays(t *testing.T) {
	th := mirror.NewThrottle(5, 5*time.Second)
	th.SetBackoff("ch", 500*time.Millisecond)

	done := make(chan struct{})
	start := time.Now()
	go func() {
		th.Wait("ch")
		close(done)
	}()
	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed < 400*time.Millisecond {
			t.Errorf("backoff resolved too fast: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backoff never resolved")
	}
}

func TestThrottle_IndependentChannels(t *testing.T) {
	th := mirror.NewThrottle(1, 5*time.Second)
	th.Wait("ch-a")
	// ch-a is now at limit; ch-b should still be immediately available
	done := make(chan struct{})
	go func() {
		th.Wait("ch-b")
		close(done)
	}()
	select {
	case <-done:
		// Good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch-b should not have been throttled by ch-a's limit")
	}
}

func repeat(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

func endsWith(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
