package agentchat

import (
	"context"
	"encoding/json"
	"testing"

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

func TestApprovalOutcome(t *testing.T) {
	assert.Equal(t, "✅ Allowed.", approvalOutcome(decisionAllow, "", defaultApprovalWait))
	assert.Equal(t, "❌ Denied: a &lt;b&gt;", approvalOutcome(decisionDeny, "a <b>", defaultApprovalWait))
	assert.Contains(t, approvalOutcome("", "", defaultApprovalWait), "No answer after 10m0s")
}

func TestApprovalBlocksCarryTheID(t *testing.T) {
	blocks := approvalBlocks("text", "abc123", defaultApprovalWait)
	data, _ := json.Marshal(blocks)
	for _, d := range []string{decisionAllow, decisionDeny, decisionTerminal} {
		assert.Contains(t, string(data), `"action_id":"`+approvalActionPrefix+d+`","value":"abc123"`)
	}
}

func clickPayload(user, decision, id string) []byte {
	return []byte(`{"type":"block_actions","user":{"id":"` + user + `"},"actions":[{"action_id":"` + approvalActionPrefix + decision + `","value":"` + id + `"}]}`)
}

func takeApproval(t *testing.T, l *Listener, id string) (string, string) {
	resp := l.Control(context.Background(), ControlRequest{Op: "approval", Approval: id})
	require.True(t, resp.OK)
	return resp.Decision, resp.Text
}

// Only the owner's click decides, the first click wins, and the hook takes
// the decision through the control socket once.
func TestListenerApprovalClicks(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	l.HandleInteraction(clickPayload("UMI", decisionAllow, "a1"))
	d, _ := takeApproval(t, l, "a1")
	assert.Equal(t, "", d, "not the owner")

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

	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.6", ThreadTS: "1999.1", User: "UBR", Text: "other thread"})
	assert.Len(t, del.got, 1, "other threads are delivered as usual")
}
