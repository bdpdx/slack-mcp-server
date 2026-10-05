package agentchat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/slack-go/slack"
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
	} {
		d, r := ParseApprovalReply(tc.text)
		assert.Equal(t, tc.decision, d, tc.text)
		assert.Equal(t, tc.reason, r, tc.text)
	}
}

func TestOwnerReplyDecision(t *testing.T) {
	msgs := []slack.Message{
		{Msg: slack.Msg{User: "UCL", Text: "parent: needs approval"}},
		{Msg: slack.Msg{User: "UMI", Text: "yes"}},
		{Msg: slack.Msg{User: "UBR", Text: "no, not now"}},
		{Msg: slack.Msg{User: "UBR", Text: "yes"}},
	}
	d, r, ok := ownerReplyDecision(msgs, "UBR")
	assert.True(t, ok)
	assert.Equal(t, decisionDeny, d, "only the owner's first reply counts")
	assert.Equal(t, "not now", r)

	_, _, ok = ownerReplyDecision(msgs[:2], "UBR")
	assert.False(t, ok)
	_, _, ok = ownerReplyDecision([]slack.Message{{Msg: slack.Msg{User: "UBR", Text: "yes"}}}, "UBR")
	assert.False(t, ok, "the parent is never a reply")
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

// Only the owner's click decides, the first click wins, and the hook takes
// the decision through the control socket once.
func TestListenerApprovalClicks(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	ctx := context.Background()
	l.HandleInteraction(clickPayload("UMI", decisionAllow, "a1"))
	assert.Equal(t, "", l.Control(ctx, ControlRequest{Op: "approval", Approval: "a1"}).Decision, "not the owner")

	l.HandleInteraction(clickPayload("UBR", decisionDeny, "a1"))
	l.HandleInteraction(clickPayload("UBR", decisionAllow, "a1"))
	resp := l.Control(ctx, ControlRequest{Op: "approval", Approval: "a1"})
	require.True(t, resp.OK)
	assert.Equal(t, decisionDeny, resp.Decision)
	assert.Equal(t, "", l.Control(ctx, ControlRequest{Op: "approval", Approval: "a1"}).Decision, "taken once")

	l.HandleInteraction([]byte(`{"type":"view_submission"}`))
	l.HandleInteraction([]byte(`not json`))
	l.HandleInteraction(clickPayload("UBR", decisionAllow, ""))
	assert.Empty(t, l.approved)
}
