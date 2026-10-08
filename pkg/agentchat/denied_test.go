package agentchat

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatDenied(t *testing.T) {
	msg, unsafe := FormatDenied("UBR", "claude-b", "Bash", []byte(`{"command":"git push origin main"}`), "[Modify Shared Resources]")
	assert.Equal(t, "", unsafe)
	assert.True(t, strings.HasPrefix(msg, "<@UBR> Auto mode blocked claude-b: Bash\n```git push origin main```"), msg)
	assert.True(t, strings.HasSuffix(msg, "\nReason: [Modify Shared Resources]"), msg)
	assert.NotContains(t, msg, "needs your approval")

	_, unsafe = FormatDenied("UBR", "claude-b", "Bash", []byte(`{"command":"ls"}`), "fine‮reversed")
	assert.Contains(t, unsafe, "its reason contains invisible")
	_, unsafe = FormatDenied("UBR", "claude-b", "Bash", []byte(`{"command":"ls​"}`), "[X]")
	assert.Contains(t, unsafe, "invisible", "the command's own hidden characters still count")
}

func TestDeniedBlocksOmitApproveWhenUnsafe(t *testing.T) {
	ids := func(unsafe string) []string {
		data, err := json.Marshal(deniedBlocks("text", unsafe, "id1", 10*time.Minute))
		require.NoError(t, err)
		var out []string
		for _, d := range []string{decisionAllow, decisionDeny, decisionTerminal} {
			if bytes.Contains(data, []byte(`"`+approvalActionPrefix+d+`"`)) {
				out = append(out, d)
			}
		}
		return out
	}
	assert.Equal(t, []string{decisionAllow, decisionDeny}, ids(""), "Approve and Decline; no terminal to send it to")
	assert.Equal(t, []string{decisionDeny}, ids("it is too long"))
}

func TestDeniedOutcomeAndRetryOutput(t *testing.T) {
	assert.Equal(t, "✅ Approved; the agent may retry.", deniedOutcome(decisionAllow, "", time.Minute))
	assert.Equal(t, "❌ Declined: use a PR", deniedOutcome(decisionDeny, "use a PR", time.Minute))
	assert.Equal(t, "⏱ No answer after 10m0s; the block stands.", deniedOutcome("", "", 10*time.Minute))
	data, _ := json.Marshal(retryDecision())
	assert.JSONEq(t, `{"hookSpecificOutput":{"hookEventName":"PermissionDenied","retry":true}}`, string(data))
}

// Claude Code ignores retry for denials without a classifier verdict, so the
// hook asks nothing (and touches neither Slack nor the listener).
func TestDeniedHookSkipsDenialsWithoutAVerdict(t *testing.T) {
	out := &bytes.Buffer{}
	c := &cli{stdout: out, stderr: &bytes.Buffer{}}
	for _, reason := range []string{
		"Auto mode could not evaluate this action and is blocking it for safety.",
		"Classifier unavailable",
	} {
		assert.Equal(t, 0, c.deniedHook(context.Background(), hookEvent{ToolName: "Bash", Reason: reason}, time.Minute))
	}
	assert.Empty(t, out.String())
}

// A request whose hook stops polling (the terminal answered and the host
// stopped it, or the session ended) is redrawn once without buttons.
func TestSweepApprovalsRedrawsEndedRequests(t *testing.T) {
	api := newFakeSlack()
	l := newTestListener(t, api, &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	ctx := context.Background()
	for _, id := range []string{"live", "gone", "late", "done"} {
		require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: id, Channel: "C1", TS: "ts-" + id, Text: "request " + id}).OK)
	}
	l.HandleInteraction(clickOn("UBR", decisionDeny, "done", "C1", "ts-done", "BCL"))
	d, _ := takeApproval(t, l, "done") // the hook took its answer and finished
	require.Equal(t, decisionDeny, d)

	now = now.Add(10 * time.Second)
	l.SweepApprovals(ctx)
	assert.Empty(t, api.updates(), "every hook polled within the window")

	now = now.Add(10 * time.Second)
	takeApproval(t, l, "live") // still polling
	l.HandleInteraction(clickOn("UBR", decisionAllow, "late", "C1", "ts-late", "BCL"))
	l.SweepApprovals(ctx)
	got := api.updates()
	require.Len(t, got, 2)
	assert.Contains(t, got, "C1|ts-gone|↩️ No longer waiting: it was answered in the terminal, or the request ended. Nothing was decided here.")
	assert.Contains(t, got, "C1|ts-late|↩️ Your answer arrived after the request had ended, so it changed nothing.")

	now = now.Add(time.Minute)
	takeApproval(t, l, "live")
	l.SweepApprovals(ctx)
	assert.Len(t, api.updates(), 2, "each ended request is redrawn once; a live or taken one never")
}
