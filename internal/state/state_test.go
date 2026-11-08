package state_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/mirror"
	"github.com/syedmusaasad/LLMDiscordRelay/internal/state"
)

func TestStoreSetGet(t *testing.T) {
	s := state.New()
	ss := &state.SessionState{
		SessionID:       "sess-test",
		DiscordThreadID: "thread1",
	}
	s.Set(ss)
	got := s.Get("sess-test")
	if got == nil {
		t.Fatal("expected session state, got nil")
	}
	if got.SessionID != "sess-test" {
		t.Errorf("got %q want sess-test", got.SessionID)
	}
}

// TestTTL_NeverSweepsLiveWindow guards restart retention: age alone is never
// enough to discard a session record while its tmux target remains alive.
func TestTTL_NeverSweepsLiveWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	old := time.Now().Add(-31 * 24 * time.Hour).UnixMilli()
	if err := os.WriteFile(path, []byte(`[{"session_id":"sess-live","tmux_target":"relay:0.0","updated_at":`+strconv.FormatInt(old, 10)+`}]`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := mirror.LoadSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SweepExpired(30*24*time.Hour, func(target string) bool { return target == "relay:0.0" }, time.Now()); err != nil {
		t.Fatal(err)
	}
	if store.Get("sess-live") == nil {
		t.Fatal("TTL sweep removed a record whose window is alive")
	}
}

func TestStoreDelete(t *testing.T) {
	s := state.New()
	s.Set(&state.SessionState{SessionID: "sess-a"})
	s.Delete("sess-a")
	if s.Get("sess-a") != nil {
		t.Error("expected nil after delete")
	}
}

func TestStoreAll(t *testing.T) {
	s := state.New()
	s.Set(&state.SessionState{SessionID: "sess-1"})
	s.Set(&state.SessionState{SessionID: "sess-2"})
	all := s.All()
	if len(all) != 2 {
		t.Errorf("got %d want 2", len(all))
	}
}
