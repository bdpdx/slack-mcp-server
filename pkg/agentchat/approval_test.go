package agentchat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseApprovalReply(t *testing.T) {
	for _, tc := range []struct{ text, decision, reason string }{
		{"yes", decisionAllow, ""},
		{"Yes!", decisionAllow, ""},
		{"<@UCL> ok go ahead", decisionAllow, ""},
		{"LGTM", decisionAllow, ""},
		{"no", decisionDeny, ""},
		{"No, use `npm ci` instead", decisionDeny, "use `npm ci` instead"},
		{"deny - wrong branch", decisionDeny, "wrong branch"},
		{"terminal", decisionTerminal, ""},
		{"what does this script do?", decisionDeny, "what does this script do?"},
		{"yes\ngo ahead", decisionAllow, ""},
		{"*yes*", decisionAllow, ""},
		{"👍", decisionAllow, ""},
		{":+1:", decisionAllow, ""},
		{"no\nwrong branch", decisionDeny, "wrong branch"},
		{":x: not yet", decisionDeny, "not yet"},
	} {
		d, r := ParseApprovalReply(tc.text)
		assert.Equal(t, tc.decision, d, tc.text)
		assert.Equal(t, tc.reason, r, tc.text)
	}
}

func TestPermissionDecisionOutput(t *testing.T) {
	data, _ := json.Marshal(permissionDecision(decisionAllow, ""))
	assert.JSONEq(t, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`, string(data))
	data, _ = json.Marshal(permissionDecision(decisionDeny, "wrong branch"))
	assert.JSONEq(t, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"The user denied this in Slack: wrong branch"}}}`, string(data))
}

// The project-channel notice names the agent, the tool and the wait, and
// never the tool's input: peers need to know an agent is stuck, not what it
// was about to run.
func TestBlockedNotice(t *testing.T) {
	assert.Equal(t, "BLOCKED: claude has waited 10m0s for approval of Bash; it is now waiting at the terminal.",
		blockedNotice("claude", "Bash", defaultApprovalWait))
	assert.Equal(t, "BLOCKED: codex-b has waited 10m0s for approval of a tool; it is now waiting at the terminal.",
		blockedNotice("codex-b", "", defaultApprovalWait))
	assert.Equal(t, "BLOCKED: claude has waited 10m0s for approval of Bash &lt;x&gt;; it is now waiting at the terminal.",
		blockedNotice("claude", "Bash <x>", defaultApprovalWait), "Slack markup is escaped")
}

func TestApprovalOutcome(t *testing.T) {
	assert.Equal(t, "✅ Allowed.", approvalOutcome(decisionAllow, "", defaultApprovalWait))
	assert.Equal(t, "❌ Denied: a &lt;b&gt;", approvalOutcome(decisionDeny, "a <b>", defaultApprovalWait))
	assert.Contains(t, approvalOutcome("", "", defaultApprovalWait), "No answer after 10m0s")
}

func TestApprovalBlocksCarryTheID(t *testing.T) {
	blocks := approvalBlocks("text", "", "abc123", defaultApprovalWait, approvalOptions{})
	data, _ := json.Marshal(blocks)
	for _, d := range []string{decisionAllow, decisionDeny, decisionTerminal} {
		assert.Contains(t, string(data), `"action_id":"`+approvalActionPrefix+d+`","value":"abc123"`)
	}
}

func clickPayload(user, decision, id string) []byte {
	return clickOn(user, decision, id, "C1", "2000.1", "BCL")
}

func clickOn(user, decision, id, channel, ts, bot string) []byte {
	return []byte(`{"type":"block_actions","user":{"id":"` + user + `"},` +
		`"container":{"type":"message","channel_id":"` + channel + `","message_ts":"` + ts + `"},` +
		`"message":{"bot_id":"` + bot + `"},` +
		`"actions":[{"action_id":"` + approvalActionPrefix + decision + `","value":"` + id + `"}]}`)
}

func takeApproval(t *testing.T, l *Listener, id string) (string, string) {
	resp := l.Control(context.Background(), ControlRequest{Op: "approval", Approval: id})
	require.True(t, resp.OK)
	return resp.Decision, resp.Text
}

// Only the owner's click on this bot's registered message, with a known
// decision, decides; the first click wins; the hook takes it once.
func TestListenerApprovalClicks(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	ctx := context.Background()
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a1", Channel: "C1", TS: "2000.1"}).OK)

	l.HandleInteraction(clickPayload("UMI", decisionAllow, "a1"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "a1", "C1", "2000.1", "BOTHER"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "a1", "C1", "2000.9", "BCL"))
	l.HandleInteraction(clickOn("UBR", "everything", "a1", "C1", "2000.1", "BCL"))
	d, _ := takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "not the owner, another bot's message, another message, unknown decision")
	now = now.Add(clickCopyWait) // the click on another message never got its copy registered

	l.HandleInteraction(clickPayload("UBR", decisionDeny, "a1"))
	l.HandleInteraction(clickPayload("UBR", decisionAllow, "a1"))
	d, _ = takeApproval(t, l, "a1")
	assert.Equal(t, decisionDeny, d)
	d, _ = takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "taken once")

	l.HandleInteraction([]byte(`{"type":"view_submission"}`))
	l.HandleInteraction([]byte(`not json`))
	l.HandleInteraction(clickPayload("UBR", decisionAllow, ""))
	d, _ = takeApproval(t, l, "")
	assert.Equal(t, "", d)
}

// A click that beats the hook's registration is kept and checked against the
// message once it registers.
func TestListenerApprovalEarlyClick(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	ctx := context.Background()
	l.HandleInteraction(clickPayload("UBR", decisionAllow, "a1"))
	d, _ := takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "not yet registered")
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a1", Channel: "C1", TS: "2000.1"}).OK)
	d, _ = takeApproval(t, l, "a1")
	assert.Equal(t, decisionAllow, d)

	l.HandleInteraction(clickOn("UBR", decisionAllow, "a2", "C1", "2000.7", "BCL"))
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a2", Channel: "C1", TS: "2000.8"}).OK)
	d, _ = takeApproval(t, l, "a2")
	assert.Equal(t, "", d, "an early click on another message does not count")
}

// Replies in an approval thread answer it and are never delivered as
// notices. A reply cannot allow, since agents can post as the owner.
func TestListenerApprovalReplies(t *testing.T) {
	api, del := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, del)
	ctx := context.Background()
	require.NoError(t, l.Subscribe(ctx, claudeSub("s1"), 0))
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a1", Channel: "C1", TS: "2000.1"}).OK)

	reply := func(ts, user, text string) {
		l.HandleMessage(ctx, Message{Channel: "C1", TS: ts, ThreadTS: "2000.1", User: user, Text: text})
	}
	reply("2000.2", "UBR", "yes go ahead")
	d, _ := takeApproval(t, l, "a1")
	assert.Equal(t, decisionHint, d, "an allow word only earns a hint")
	d, _ = takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "the hint is given once")

	reply("2000.3", "UMI", "no")
	d, _ = takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "only the owner answers")

	reply("2000.4", "UBR", "no, use the staging db")
	d, r := takeApproval(t, l, "a1")
	assert.Equal(t, decisionDeny, d)
	assert.Equal(t, "use the staging db", r)

	reply("2000.5", "UBR", "terminal")
	d, _ = takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "answered already")
	assert.Empty(t, del.got, "approval-thread replies never reach the session")
	for _, ts := range []string{"2000.2", "2000.3", "2000.4", "2000.5"} {
		assert.True(t, l.state.WasDelivered("s1", "C1", ts), "consumed reply %s is marked delivered, so a restarted listener's recovery skips it", ts)
	}

	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.6", ThreadTS: "1999.1", User: "UBR", Text: "other thread"})
	assert.Len(t, del.got, 1, "other threads are delivered as usual")
}

// While a request waits, the owner can answer it in the channel itself with
// a recognised opening word; anything else there is an ordinary message.
func TestListenerApprovalChannelReplies(t *testing.T) {
	api, del := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, del)
	ctx := context.Background()
	require.NoError(t, l.Subscribe(ctx, claudeSub("s1"), 0))
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "old", Channel: "C1", TS: "2000.1"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "new", Channel: "C1", TS: "2000.2"}).OK)
	post := func(ts, user, text string) {
		l.HandleMessage(ctx, Message{Channel: "C1", TS: ts, User: user, Text: text})
	}

	post("2000.3", "UBR", "what does that script do?")
	assert.Len(t, del.got, 1, "an ordinary message is delivered and answers nothing")
	d, _ := takeApproval(t, l, "new")
	assert.Equal(t, "", d)

	post("2000.4", "UMI", "no")
	d, _ = takeApproval(t, l, "new")
	assert.Equal(t, "", d, "only the owner answers")

	post("2000.5", "UBR", "yes")
	d, _ = takeApproval(t, l, "new")
	assert.Equal(t, decisionHint, d, "typed approval only earns the hint")

	post("2000.6", "UBR", "No, use the staging db instead")
	d, r := takeApproval(t, l, "new")
	assert.Equal(t, decisionDeny, d, "the newest waiting request is answered")
	assert.Equal(t, "use the staging db instead", r)
	d, _ = takeApproval(t, l, "old")
	assert.Equal(t, "", d)

	post("2000.7", "UBR", "terminal")
	d, _ = takeApproval(t, l, "old")
	assert.Equal(t, decisionTerminal, d, "then the next waiting one")

	post("2000.8", "UBR", "no worries, carry on")
	assert.Len(t, del.got, 3, "with nothing waiting, even a deny word is an ordinary message (UMI's no was delivered too)")
}
