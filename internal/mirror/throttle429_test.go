package mirror

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// rateLimitTestError is a typed 429 error for use in unit tests.
// It implements the retryAfterDurationGetter interface checked by extractRetryAfter.
type rateLimitTestError struct {
	retryAfter time.Duration
	channel    string
}

func (e *rateLimitTestError) Error() string {
	return fmt.Sprintf("Rate limit exceeded on %s, retry after %s", e.channel, e.retryAfter)
}

func (e *rateLimitTestError) RetryAfterDuration() time.Duration {
	return e.retryAfter
}

// mock429DiscordClient is a DiscordClient where SendMessage returns a typed
// *discordgo.RateLimitError on the first call and succeeds on subsequent calls.
// This exercises the real typed 429 path in extractRetryAfter and discordSend.
type mock429DiscordClient struct {
	calls   int
	sent    []string
	retryMS int64 // retry_after in milliseconds
}

// makeRateLimitError constructs a real *discordgo.RateLimitError with the given
// retry duration and URL, matching exactly the type returned by discordgo on a
// live 429 response.
func makeRateLimitError(d time.Duration, url string) *discordgo.RateLimitError {
	return &discordgo.RateLimitError{
		RateLimit: &discordgo.RateLimit{
			TooManyRequests: &discordgo.TooManyRequests{RetryAfter: d},
			URL:             url,
		},
	}
}

func (m *mock429DiscordClient) SendMessage(channelID, content string) (MsgRef, error) {
	m.calls++
	if m.calls == 1 {
		// Return a real *discordgo.RateLimitError so extractRetryAfter exercises
		// the primary typed path (not the test-double interface fallback).
		d := time.Duration(m.retryMS) * time.Millisecond
		return MsgRef{}, makeRateLimitError(d, "/api/v10/channels/"+channelID+"/messages")
	}
	m.sent = append(m.sent, content)
	return MsgRef{ID: fmt.Sprintf("msg-%d", m.calls), ChannelID: channelID}, nil
}

func (m *mock429DiscordClient) EditMessage(channelID, messageID, content string) error {
	m.calls++
	if m.calls == 1 {
		d := time.Duration(m.retryMS) * time.Millisecond
		return makeRateLimitError(d, "/api/v10/channels/"+channelID+"/messages/"+messageID)
	}
	return nil
}

func (m *mock429DiscordClient) CreateThread(channelID, messageID, name string) (ThreadRef, error) {
	return ThreadRef{ID: "thread-1"}, nil
}

func (m *mock429DiscordClient) SendTyping(channelID string) error { return nil }

func (m *mock429DiscordClient) RenameThread(threadID, name string) error { return nil }

func (m *mock429DiscordClient) OnMessage(fn MessageHandler) {}

func (m *mock429DiscordClient) SendComponents(channelID, content string, components []Component) (MsgRef, error) {
	return m.SendMessage(channelID, content)
}

func (m *mock429DiscordClient) EditComponents(channelID, messageID, content string, components []Component) error {
	return m.EditMessage(channelID, messageID, content)
}

func (m *mock429DiscordClient) OnInteraction(fn InteractionHandler) {}

func (m *mock429DiscordClient) AcknowledgeInteraction(InteractionEvent) error { return nil }

// TestThrottle_429Backoff verifies that when a Discord send returns a typed
// *discordgo.RateLimitError (RetryAfter), the watcher:
//  1. Extracts the retry_after duration via the *discordgo.RateLimitError primary path.
//  2. Feeds it into the Throttle via SetBackoff.
//  3. Sleeps for at least retry_after.
//  4. Retries the send once and succeeds.
func TestThrottle_429Backoff(t *testing.T) {
	const retryAfterMS = 100 // small value to keep the test fast

	mock := &mock429DiscordClient{retryMS: retryAfterMS}

	// Build a minimal Watcher. We don't need DB or full config for this unit test.
	w := &Watcher{
		discord:  mock,
		throttle: NewThrottle(10, 5*time.Second),
		sessions: &Sessions{path: t.TempDir() + "/sessions.json"},
	}

	channelID := "ch-test-429"
	content := "hello world"

	start := time.Now()
	w.throttle.Wait(channelID) // consume the pre-send throttle slot as discordSend would
	msg, err := w.discordSend(channelID, content)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected discordSend to succeed after retry, got: %v", err)
	}
	if msg.ID == "" {
		t.Error("expected a non-empty message ID after retry")
	}
	if mock.calls != 2 {
		t.Errorf("expected exactly 2 calls to SendMessage (1 429 + 1 retry), got %d", mock.calls)
	}
	minDelay := time.Duration(retryAfterMS) * time.Millisecond
	if elapsed < minDelay/2 {
		// Allow 50% slack for clock granularity.
		t.Errorf("expected at least %v sleep for 429 backoff, but total elapsed was only %v", minDelay, elapsed)
	}
	t.Logf("✓ 429 backoff: %d calls, elapsed=%v (retry_after=%v)", mock.calls, elapsed.Round(time.Millisecond), minDelay)
}

// TestComponentWritesUseThrottleAnd429Retry ensures component controls cannot
// sidestep the same pacing/backoff path used by ordinary Discord writes.
func TestComponentWritesUseThrottleAnd429Retry(t *testing.T) {
	mock := &mock429DiscordClient{retryMS: 20}
	w := &Watcher{
		discord:  mock,
		throttle: NewThrottle(1, time.Millisecond),
		sessions: &Sessions{path: t.TempDir() + "/sessions.json"},
	}
	start := time.Now()
	if _, err := w.discordSendComponents("component-429", "permission", nil); err != nil {
		t.Fatalf("component send: %v", err)
	}
	if mock.calls != 2 {
		t.Fatalf("component send calls = %d, want 2 (429 then retry)", mock.calls)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("component send did not wait for 429 backoff")
	}
}

// TestThrottle_429BackoffCancellation verifies a real relay shutdown does not
// remain blocked in a Discord RetryAfter sleep. The retry is not issued after
// cancellation and the channel backoff is cleared.
func TestThrottle_429BackoffCancellation(t *testing.T) {
	mock := &mock429DiscordClient{retryMS: 5_000}
	w := &Watcher{
		discord:  mock,
		throttle: NewThrottle(10, 5*time.Second),
		sessions: &Sessions{path: t.TempDir() + "/sessions.json"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := w.discordSendContext(ctx, "ch-cancel-429", "hello")
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for mock.calls == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if mock.calls != 1 {
		t.Fatalf("expected first request to return 429, got %d calls", mock.calls)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("429 wait did not stop when relay context was cancelled")
	}
	if mock.calls != 1 {
		t.Fatalf("expected no retry after cancellation, got %d calls", mock.calls)
	}
}

// TestExtractRetryAfter verifies the typed and string-fallback paths for rate-limit errors.
func TestExtractRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   time.Duration
		wantOK bool
	}{
		{
			name:   "typed rateLimitTestError",
			err:    &rateLimitTestError{retryAfter: 500 * time.Millisecond},
			want:   500 * time.Millisecond,
			wantOK: true,
		},
		{
			name:   "string fallback 500ms",
			err:    fmt.Errorf("Rate limit exceeded on /api/v10/channels/123/messages, retry after 500ms"),
			want:   500 * time.Millisecond,
			wantOK: true,
		},
		{
			name:   "string fallback 2s",
			err:    fmt.Errorf("Rate limit exceeded on /api/v10/channels/123/messages, retry after 2s"),
			want:   2 * time.Second,
			wantOK: true,
		},
		{
			name:   "unrelated error",
			err:    fmt.Errorf("some other error"),
			want:   0,
			wantOK: false,
		},
		{
			name:   "nil error",
			err:    nil,
			want:   0,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractRetryAfter(tc.err)
			if ok != tc.wantOK {
				t.Errorf("extractRetryAfter(%v): ok=%v, want %v", tc.err, ok, tc.wantOK)
				return
			}
			if ok && got != tc.want {
				t.Errorf("extractRetryAfter(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
