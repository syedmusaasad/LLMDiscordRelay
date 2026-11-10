package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
)

const trustDialogText = "Quick safety check: Is this a project you created or one you trust?\n\n❯ No, exit\n  Yes, I trust this folder\n"

// keyRecorderPane starts a pane that appends every raw byte it receives to the
// returned file, so tests can assert exactly which keys were sent.
func keyRecorderPane(t *testing.T, name string) (target, file string) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "keys")
	target = tmuxPane(t, name, `stty raw -echo; exec cat >'`+file+`'`)
	time.Sleep(300 * time.Millisecond) // let stty take effect before keys arrive
	return target, file
}

func recordedKeys(file string) string {
	b, _ := os.ReadFile(file)
	return string(b)
}

func TestClaimIsAtomicAcrossPair(t *testing.T) {
	var p pendingInteractionStore
	e := pendingInteraction{target: "t", threadID: "c", messageID: "m1", kind: "approval"}
	other := pendingInteraction{target: "t", threadID: "c", messageID: "m2", kind: "approval"}
	for round := 0; round < 200; round++ {
		p.put("a", e)
		p.put("d", e)
		p.put("x", other)
		var wins int32
		var wg sync.WaitGroup
		for _, id := range []string{"a", "d", "a", "d"} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if _, ids, ok := p.claim(id); ok {
					if len(ids) != 2 {
						t.Errorf("claim removed %v, want the pair", ids)
					}
					atomic.AddInt32(&wins, 1)
				}
			}(id)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d claimers won, want exactly 1", round, wins)
		}
		if _, _, ok := p.claim("x"); !ok {
			t.Fatal("claim of one message removed another message's entry")
		}
	}
}

// Concurrent Approve and Deny clicks must send exactly one decision.
func TestConcurrentApproveDenyClicksSendOneKey(t *testing.T) {
	target, file := keyRecorderPane(t, "llm-test-conc")
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.AllowedUserID = "me"
	w.cfg.ApprovalTimeout = time.Minute
	fp := &fakePane{text: permPane("rm -rf x")}
	w.capturePane = fp.capture
	sig := inject.PermissionSignature(fp.text)

	for round := 0; round < 5; round++ {
		before := len(recordedKeys(file))
		if !w.postApproval("chan", target, sig) {
			t.Fatal("postApproval failed")
		}
		approveID, denyID := lastComponentIDs(t, dc)
		var wg sync.WaitGroup
		for _, id := range []string{approveID, denyID, approveID, denyID} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				w.handleInteraction(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: id})
			}(id)
		}
		wg.Wait()
		time.Sleep(300 * time.Millisecond)
		got := recordedKeys(file)[before:]
		if got != "1" && got != "\x1b" {
			t.Fatalf("round %d: pane received %q, want exactly one decision", round, got)
		}
		w.pendingInteractions.clearResolved("chan", target, "approval")
	}
}

// A click racing the approval timeout must not add a second decision.
func TestApprovalClickRacingTimeoutSendsOneKey(t *testing.T) {
	target, file := keyRecorderPane(t, "llm-test-race")
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.AllowedUserID = "me"
	fp := &fakePane{text: permPane("rm -rf x")}
	w.capturePane = fp.capture
	sig := inject.PermissionSignature(fp.text)

	for round := 0; round < 8; round++ {
		before := len(recordedKeys(file))
		w.cfg.ApprovalTimeout = 40 * time.Millisecond
		if !w.postApproval("chan", target, sig) {
			t.Fatal("postApproval failed")
		}
		approveID, denyID := lastComponentIDs(t, dc)
		id := approveID
		if round%2 == 1 {
			id = denyID
		}
		time.Sleep(time.Duration(30+round*3) * time.Millisecond)
		w.handleInteraction(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: id})
		time.Sleep(400 * time.Millisecond)
		got := recordedKeys(file)[before:]
		if len(got) > 1 {
			t.Fatalf("round %d: pane received %q, want at most one decision", round, got)
		}
		w.pendingInteractions.clearResolved("chan", target, "approval")
	}
}

// Clicking Approve/Deny after the trust dialog was answered locally but before
// the periodic retirement poll must retire the request without sending keys.
func TestTrustClickAfterLocalAnswerSendsNoKeys(t *testing.T) {
	for i, approve := range []bool{true, false} {
		target, file := keyRecorderPane(t, "llm-test-trustclick"+string(rune('a'+i)))
		dc := newRecordingClient()
		w := newTestWatcher(t, dc, t.TempDir())
		w.cfg.AllowedUserID = "me"
		w.cfg.ApprovalTimeout = time.Minute
		pane := &fakePane{}
		pane.set(trustDialogText)
		w.capturePane = pane.capture

		done := make(chan bool, 1)
		go func() { done <- w.askTrust(context.Background(), target, "chan") }()
		var approveID, denyID string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && approveID == "" {
			dc.mu.Lock()
			if len(dc.components) > 0 {
				for _, c := range dc.components[0] {
					switch {
					case strings.HasPrefix(c.CustomID, "trust:approve:"):
						approveID = c.CustomID
					case strings.HasPrefix(c.CustomID, "trust:deny:"):
						denyID = c.CustomID
					}
				}
			}
			dc.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
		if approveID == "" {
			t.Fatal("trust request not posted")
		}
		// Trusted in the terminal; the retirement poll (500 ms) has not run yet.
		pane.set(idlePaneText)
		id := denyID
		if approve {
			id = approveID
		}
		w.handleInteraction(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: id})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("askTrust did not return after the stale click")
		}
		time.Sleep(300 * time.Millisecond)
		if got := recordedKeys(file); got != "" {
			t.Fatalf("approve=%v: stale trust click sent keys %q into the pane", approve, got)
		}
		if !edited(dc, "Trust request expired") {
			t.Fatalf("approve=%v: stale request not marked expired: %q", approve, dc.final())
		}
		if w.pendingInteractions.has("chan", target, "trust") {
			t.Fatal("trust entry still pending")
		}
	}
}

// Concurrent trust clicks (and the timeout) resolve the request exactly once.
func TestConcurrentTrustClicksResolveOnce(t *testing.T) {
	target, file := keyRecorderPane(t, "llm-test-trustconc")
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.AllowedUserID = "me"
	w.cfg.ApprovalTimeout = 150 * time.Millisecond
	pane := &fakePane{}
	pane.set(trustDialogText)
	w.capturePane = pane.capture

	done := make(chan bool, 1)
	go func() { done <- w.askTrust(context.Background(), target, "chan") }()
	var approveID, denyID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && approveID == "" {
		dc.mu.Lock()
		if len(dc.components) > 0 {
			for _, c := range dc.components[0] {
				if strings.HasPrefix(c.CustomID, "trust:approve:") {
					approveID = c.CustomID
				} else if strings.HasPrefix(c.CustomID, "trust:deny:") {
					denyID = c.CustomID
				}
			}
		}
		dc.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if approveID == "" {
		t.Fatal("trust request not posted")
	}
	time.Sleep(140 * time.Millisecond) // land the clicks around the timeout
	var wg sync.WaitGroup
	for _, id := range []string{approveID, denyID, approveID, denyID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			w.handleInteraction(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: id})
		}(id)
	}
	wg.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("askTrust did not return")
	}
	time.Sleep(300 * time.Millisecond)
	got := recordedKeys(file)
	if got != "\x1b" && got != "\x1b[B\r" {
		t.Fatalf("pane received %q, want one decision (Esc, or Down+Enter)", got)
	}
}
