package mirror

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
)

// failingComponentsClient fails the first n SendComponents calls.
type failingComponentsClient struct {
	*recordingClient
	failures int32
}

func (c *failingComponentsClient) SendComponents(channelID, content string, comps []Component) (MsgRef, error) {
	if atomic.AddInt32(&c.failures, -1) >= 0 {
		return MsgRef{}, errTestSend
	}
	return c.recordingClient.SendComponents(channelID, content, comps)
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errTestSend = testErr("discord down")

func lastComponentIDs(t *testing.T, dc *recordingClient) (approve, deny string) {
	t.Helper()
	dc.mu.Lock()
	defer dc.mu.Unlock()
	comps := dc.components[len(dc.components)-1]
	for _, c := range comps {
		switch {
		case strings.HasPrefix(c.CustomID, "approval:approve:"):
			approve = c.CustomID
		case strings.HasPrefix(c.CustomID, "approval:deny:"):
			deny = c.CustomID
		}
	}
	return
}

func TestApprovalButtonsShareOneIDAndClearTogether(t *testing.T) {
	target := tmuxPane(t, "llm-test-appr", `sleep 30`)
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.AllowedUserID = "me"
	dc.OnInteraction(w.handleInteraction)

	for i, deny := range []bool{false, true, false} {
		if !w.postApproval("chan", target, "rm -rf x") {
			t.Fatalf("round %d: postApproval failed", i)
		}
		approveID, denyID := lastComponentIDs(t, dc)
		if approveID == "" || denyID == "" {
			t.Fatalf("round %d: missing buttons", i)
		}
		if strings.TrimPrefix(approveID, "approval:approve:") != strings.TrimPrefix(denyID, "approval:deny:") {
			t.Fatalf("round %d: ids do not share a stamp: %s %s", i, approveID, denyID)
		}
		if !w.pendingInteractions.has("chan", target, "approval") {
			t.Fatalf("round %d: approval not pending", i)
		}
		click := approveID
		if deny {
			click = denyID
		}
		dc.onInter(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: click})
		if w.pendingInteractions.has("chan", target, "approval") {
			t.Fatalf("round %d: sibling button survived resolution (deny=%v)", i, deny)
		}
	}
}

func TestApprovalTimerDisarmedByResolution(t *testing.T) {
	target := tmuxPane(t, "llm-test-apprtimer", `sleep 30`)
	dc := newRecordingClient()
	w := newTestWatcher(t, dc, t.TempDir())
	w.cfg.AllowedUserID = "me"
	w.cfg.ApprovalTimeout = 300 * time.Millisecond
	dc.OnInteraction(w.handleInteraction)
	if !w.postApproval("chan", target, "x") {
		t.Fatal("postApproval failed")
	}
	_, denyID := lastComponentIDs(t, dc)
	dc.onInter(InteractionEvent{ChannelID: "chan", AuthorID: "me", CustomID: denyID})
	time.Sleep(700 * time.Millisecond)
	dc.mu.Lock()
	defer dc.mu.Unlock()
	for _, e := range dc.edits {
		if strings.Contains(e, "automatically") {
			t.Fatalf("timeout fired after manual deny: %q", e)
		}
	}
}

func TestPostFailureReportsNotDelivered(t *testing.T) {
	target := tmuxPane(t, "llm-test-postfail", `sleep 30`)
	dc := &failingComponentsClient{recordingClient: newRecordingClient(), failures: 2}
	w := newTestWatcher(t, dc, t.TempDir())
	if w.postApproval("chan", target, "x") {
		t.Fatal("postApproval reported success on delivery failure")
	}
	if w.pendingInteractions.has("chan", target, "approval") {
		t.Fatal("failed post left a pending entry")
	}
	q := inject.Question{Prompt: "Color?", Options: []string{"Red", "Green"}}
	if w.postQuestion("chan", target, q) {
		t.Fatal("postQuestion reported success on delivery failure")
	}
	// Recovery: the same prompt posts fine once Discord is back.
	if !w.postApproval("chan", target, "x") || !w.postQuestion("chan", target, q) {
		t.Fatal("retry after recovery failed")
	}
	if !w.pendingInteractions.has("chan", target, "approval") || !w.pendingInteractions.has("chan", target, "question") {
		t.Fatal("recovered posts not pending")
	}
}
