package agentchat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session watching several projects has a direct channel in each; a
// request goes to all of them.
func TestDirectChannelsOf(t *testing.T) {
	ids := []string{"CP2", "CD2", "CP1", "CD1", "CS1"}
	names := map[string]string{
		"CP1": "beta", "CD1": "beta__brian_claude", "CS1": "beta__claude_codex",
		"CP2": "alpha", "CD2": "alpha__brian_claude",
	}
	got := directChannelsOf(ids, names, "brian", "claude")
	assert.Equal(t, []namedChannel{{"CD2", "alpha__brian_claude"}, {"CD1", "beta__brian_claude"}}, got,
		"every watched project's direct channel, in name order; side channels never")
	assert.Empty(t, directChannelsOf([]string{"CS1"}, names, "brian", "claude"))
	assert.Equal(t, []namedChannel{{"CD1", "beta__brian_claude"}},
		directChannelsOf([]string{"CP1", "CD1"}, names, "brian", "claude"))
}

// One request posted in two direct channels: a click, a thread reply or a
// channel reply on either copy answers it, and the first answer wins.
func TestApprovalAcrossDirectChannels(t *testing.T) {
	ctx := context.Background()
	watch := func(l *Listener, id string) {
		require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: id, Channel: "C1", TS: "2000.1", Text: "req"}).OK)
		require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: id, Channel: "C2", TS: "3000.1", Text: "req"}).OK)
	}

	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	watch(l, "a")
	l.HandleInteraction(clickOn("UBR", decisionDeny, "a", "C1", "2000.1", "BCL"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "a", "C2", "3000.1", "BCL"))
	d, _ := takeApproval(t, l, "a")
	assert.Equal(t, decisionDeny, d, "the first copy stays live after a second registers; the first click wins")

	l = newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	watch(l, "b")
	l.HandleInteraction(clickOn("UBR", decisionAllow, "b", "C2", "3000.1", "BCL"))
	d, _ = takeApproval(t, l, "b")
	assert.Equal(t, decisionAllow, d, "a click on the second copy counts")

	api, del := newFakeSlack(), &fakeDeliverer{}
	l = newTestListener(t, api, del)
	require.NoError(t, l.Subscribe(ctx, claudeSub("s1"), 0))
	watch(l, "t")
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.2", ThreadTS: "2000.1", User: "UBR", Text: "no, wrong table"})
	d, r := takeApproval(t, l, "t")
	assert.Equal(t, decisionDeny, d, "a thread reply on the first copy answers")
	assert.Equal(t, "wrong table", r)
	assert.Empty(t, del.got)

	l = newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	watch(l, "c")
	assert.True(t, l.approvalReply(Message{Channel: "C1", TS: "2000.3", User: "UBR", Text: "terminal"}))
	d, _ = takeApproval(t, l, "c")
	assert.Equal(t, decisionTerminal, d, "a channel reply beside the first copy answers")
}

// A click can reach the listener before the hook registers that copy; it
// is honored once the copy registers, even if another copy registered first.
func TestEarlyClickOnLaterCopy(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "e", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickOn("UBR", decisionAllow, "e", "C2", "3000.1", "BCL"))
	d, _ := takeApproval(t, l, "e")
	assert.Equal(t, "", d, "the clicked copy is not registered yet")
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "e", Channel: "C2", TS: "3000.1"}).OK)
	d, _ = takeApproval(t, l, "e")
	assert.Equal(t, decisionAllow, d)
}

// The hint for a typed allow word goes to the copy where it was typed.
func TestHintNamesTheCopy(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "h", Channel: "C1", TS: "2000.1"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "h", Channel: "C2", TS: "3000.1"}).OK)
	l.HandleMessage(ctx, Message{Channel: "C2", TS: "3000.2", ThreadTS: "3000.1", User: "UBR", Text: "yes"})
	resp := l.Control(ctx, ControlRequest{Op: "approval", Approval: "h"})
	assert.Equal(t, decisionHint, resp.Decision)
	assert.Equal(t, "C2", resp.Channel)
	assert.Equal(t, "3000.1", resp.TS)
}

// An ended request is redrawn in every copy, and a failed copy is retried
// until it succeeds without redrawing the others again.
func TestSweepRedrawsEveryCopy(t *testing.T) {
	api := newFakeSlack()
	l := newTestListener(t, api, &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	ctx := context.Background()
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "g", Channel: "C1", TS: "2000.1", Text: "req"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "g", Channel: "C2", TS: "3000.1", Text: "req"}).OK)

	api.failUpd = 1
	now = now.Add(31 * time.Second)
	l.SweepApprovals(ctx)
	require.Len(t, api.updates(), 1, "one copy redrawn, the other failed")

	now = now.Add(11 * time.Second)
	l.SweepApprovals(ctx)
	got := api.updates()
	require.Len(t, got, 2)
	for _, u := range got {
		assert.True(t, strings.HasSuffix(u, "Nothing was decided here."), u)
	}
	assert.NotEqual(t, strings.SplitN(got[0], "|", 2)[0], strings.SplitN(got[1], "|", 2)[0], "each copy once")

	now = now.Add(time.Hour / 2)
	l.SweepApprovals(ctx)
	assert.Len(t, api.updates(), 2, "nothing left owed")
}

// The terminal reason names every channel the question went to.
func TestAskHookReasonSeveralChannels(t *testing.T) {
	got := askHookReason([]postedMsg{{"C1", "alpha__brian_claude", "1.2"}, {"C2", "beta__brian_claude", "3.4"}})
	assert.True(t, strings.HasPrefix(got, "Question sent to Slack: #alpha__brian_claude, #beta__brian_claude. Answer it there.\n"), got)
	assert.Contains(t, got, "#alpha__brian_claude (ts 1.2)")
	assert.Contains(t, got, "#beta__brian_claude (ts 3.4)")
}

// The poll reports how many copies are registered, so a hook whose
// registration of one copy failed registers it again.
func TestApprovalPollCountsCopies(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "p", Channel: "C1", TS: "2000.1"}).OK)
	resp := l.Control(ctx, ControlRequest{Op: "approval", Approval: "p"})
	assert.False(t, resp.Unknown)
	assert.Equal(t, 1, resp.Copies)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "p", Channel: "C2", TS: "3000.1"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "p", Channel: "C2", TS: "3000.1"}).OK)
	assert.Equal(t, 2, l.Control(ctx, ControlRequest{Op: "approval", Approval: "p"}).Copies, "registering a copy again adds nothing")
}

// Clicks apply in the order they were made: an approval clicked after a
// denial never wins just because its copy registered first.
func TestClicksApplyInClickOrder(t *testing.T) {
	ctx := context.Background()
	watch := func(l *Listener, id, ch, ts string) {
		require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: id, Channel: ch, TS: ts}).OK)
	}

	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	l.HandleInteraction(clickOn("UBR", decisionDeny, "o", "C2", "3000.1", "BCL"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "o", "C1", "2000.1", "BCL"))
	watch(l, "o", "C1", "2000.1")
	d, _ := takeApproval(t, l, "o")
	assert.Equal(t, "", d, "the earlier denial waits for its copy; the later approval may not overtake it")
	watch(l, "o", "C2", "3000.1")
	d, _ = takeApproval(t, l, "o")
	assert.Equal(t, decisionDeny, d)

	l = newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	watch(l, "r", "C1", "2000.1")
	l.HandleInteraction(clickOn("UBR", decisionDeny, "r", "C2", "3000.1", "BCL"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "r", "C1", "2000.1", "BCL"))
	d, _ = takeApproval(t, l, "r")
	assert.Equal(t, "", d, "a click on a registered copy waits behind an earlier one too")
	watch(l, "r", "C2", "3000.1")
	d, _ = takeApproval(t, l, "r")
	assert.Equal(t, decisionDeny, d)
}

// Registering a copy reports how many the listener holds.
func TestWatchReportsCopies(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	assert.Equal(t, 1, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "w", Channel: "C1", TS: "2000.1"}).Copies)
	assert.Equal(t, 2, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "w", Channel: "C2", TS: "3000.1"}).Copies)
}

// Against a listener too old to hold several copies (it reports no count),
// the hook posts one copy only; a newer one gets every copy. A failed post
// or registration does not stop the rest.
func TestPostCopiesNegotiates(t *testing.T) {
	targets := []namedChannel{{"C1", "a"}, {"C2", "b"}, {"C3", "c"}}
	run := func(fail string, count func(n int) int) (posted []postedMsg, calls int) {
		n := 0
		got := postCopies(targets,
			func(t namedChannel) (string, error) {
				calls++
				if t.id == fail {
					return "", errors.New("post failed")
				}
				return t.id + "-ts", nil
			},
			func(p postedMsg) (int, error) {
				n++
				return count(n), nil
			})
		return got, calls
	}
	old, calls := run("", func(int) int { return 0 })
	assert.Equal(t, []postedMsg{{"C1", "a", "C1-ts"}}, old, "an older listener gets one copy")
	assert.Equal(t, 1, calls)

	all, _ := run("", func(n int) int { return n })
	assert.Len(t, all, 3)

	skip, _ := run("C1", func(n int) int { return n })
	assert.Equal(t, []postedMsg{{"C2", "b", "C2-ts"}, {"C3", "c", "C3-ts"}}, skip)

	unreg := postCopies(targets,
		func(t namedChannel) (string, error) { return t.id + "-ts", nil },
		func(p postedMsg) (int, error) { return 0, errors.New("no listener") })
	assert.Len(t, unreg, 3, "a failed registration is healed by the poll, not by posting fewer copies")
}

// A click whose copy never registers holds up later clicks only briefly.
func TestUnregisteredClickExpires(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "x", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickOn("UBR", decisionDeny, "x", "C9", "9000.1", "BCL"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "x", "C1", "2000.1", "BCL"))
	d, _ := takeApproval(t, l, "x")
	assert.Equal(t, "", d)
	now = now.Add(clickCopyWait)
	d, _ = takeApproval(t, l, "x")
	assert.Equal(t, decisionAllow, d, "the click on a copy that never registered is dropped")
}
