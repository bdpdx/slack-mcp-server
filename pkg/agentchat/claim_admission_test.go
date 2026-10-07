package agentchat

import (
	"context"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestClaimCheckRechecksGMAck(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	parent := msg("1800000000.000100", "UBR", "<@UCL> please decide")
	parent.Reactions = []slack.ItemReaction{{Name: "white_check_mark", Users: []string{"UCL"}}}
	f.api.replies["C1|1800000000.000100"] = []slack.Message{parent}
	assert.True(t, f.l.answeredInSlack(context.Background(), &GMWatch{Channel: "C1", TS: "1800000000.000100", GMID: "UCL"}))
	got := f.l.Control(context.Background(), ControlRequest{Op: "cohort-claim-check", SessionID: "s2", Cohort: &CohortReg{Project: "proj", Agent: "codex-b"}, Expect: &GMState{Term: 0, GM: "claude"}})
	assert.False(t, got.OK, "a processed GM ack must retire this claim deadline")
}

func TestClaimCheckRechecksLiveness(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.d.dead = map[string]bool{"s2": true}
	got := f.l.Control(context.Background(), ControlRequest{Op: "cohort-claim-check", SessionID: "s2", Cohort: &CohortReg{Project: "proj", Agent: "codex-b"}, Expect: &GMState{Term: 0, GM: "claude"}})
	assert.False(t, got.OK, "saved subscription must not stand in for fresh native liveness")
}

func TestClaimCheckFailsClosedWhenSlackIsUnreadable(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.api.failReads = true
	got := f.l.Control(context.Background(), ControlRequest{Op: "cohort-claim-check", SessionID: "s2", Cohort: &CohortReg{Project: "proj", Agent: "codex-b"}, Expect: &GMState{Term: 0, GM: "claude"}})
	assert.False(t, got.OK, "no evidence the deadline is still unanswered: refuse")
}

func TestClaimCheckUserDirectedSkipsOnlyTheDeadline(t *testing.T) {
	f := newCohortFixture(t)
	req := func(session, agent string) ControlRequest {
		return ControlRequest{Op: "cohort-claim-check", SessionID: session, UserDirected: true,
			Cohort: &CohortReg{Project: "proj", Agent: agent}, Expect: &GMState{Term: 0, GM: "claude"}}
	}
	assert.True(t, f.l.Control(context.Background(), req("s2", "codex-b")).OK, "no deadline needed on the user's word")
	f.d.dead = map[string]bool{"s2": true}
	assert.False(t, f.l.Control(context.Background(), req("s2", "codex-b")).OK, "but liveness still applies")
	f.d.dead = nil
	require.True(t, f.l.Control(context.Background(), ControlRequest{Op: "cohort-duty", SessionID: "s2", Text: "off", Cohort: &CohortReg{Project: "proj"}}).OK)
	assert.False(t, f.l.Control(context.Background(), req("s2", "codex-b")).OK, "and duty")
}
