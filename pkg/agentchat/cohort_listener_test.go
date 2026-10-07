package agentchat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cohortFixture is a listener whose sessions s2 (codex-b) and s3 (codex-r)
// watch project channel C1 ("proj"), with claude as GM.
type cohortFixture struct {
	t    *testing.T
	api  *fakeSlack
	d    *fakeDeliverer
	l    *Listener
	root string
	now  time.Time
}

func newCohortFixture(t *testing.T) *cohortFixture {
	f := &cohortFixture{t: t, api: newFakeSlack(), d: &fakeDeliverer{}, root: t.TempDir(), now: time.Unix(1_800_000_000, 0)}
	writeProject(t, f.root, "proj", "<!-- cohort-succession: claude, codex-b, codex-r -->\n")
	f.l = newTestListener(t, f.api, f.d)
	f.l.Now = func() time.Time { return f.now }
	for _, s := range []struct{ session, agent string }{{"s2", "codex-b"}, {"s3", "codex-r"}} {
		require.NoError(t, f.l.Subscribe(context.Background(), claudeSub(s.session), 0))
		f.register(s.session, s.agent)
	}
	return f
}

func (f *cohortFixture) register(session, agent string) {
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: session,
		Cohort: &CohortReg{Project: "proj", Agent: agent, Root: f.root, Channel: "C1"}})
	require.True(f.t, resp.OK, resp.Error)
}

func (f *cohortFixture) at(d time.Duration) { f.now = time.Unix(1_800_000_000, 0).Add(d) }

func (f *cohortFixture) tick() { f.l.CohortTick(context.Background()) }

func (f *cohortFixture) notices(session string) []string {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	var out []string
	for _, g := range f.d.got {
		if g.session == session && strings.Contains(g.text, "[cohort]") {
			out = append(out, g.text)
		}
	}
	return out
}

func TestCohortRegisterValidates(t *testing.T) {
	f := newCohortFixture(t)
	bad := []ControlRequest{
		{Op: "cohort-register", SessionID: "s2", Cohort: &CohortReg{Project: "proj", Agent: "mallory", Root: f.root, Channel: "C1"}},
		{Op: "cohort-register", SessionID: "s2", Cohort: &CohortReg{Project: "nope", Agent: "codex-b", Root: f.root, Channel: "C1"}},
		{Op: "cohort-register", SessionID: "s9", Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: f.root, Channel: "C1"}},
		{Op: "cohort-register", SessionID: "s2", Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: f.root, Channel: "C9"}},
	}
	for i, req := range bad {
		resp := f.l.Control(context.Background(), req)
		assert.False(t, resp.OK, "case %d should fail", i)
	}
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-status"})
	require.True(t, resp.OK)
	assert.Len(t, resp.Cohort, 2)
}

// An @mention of the GM left unanswered for 15 minutes tells the first
// successor, then 10 minutes later the next one.
func TestCohortUnansweredMentionEscalates(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})

	f.at(14 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"))

	f.at(15 * time.Minute)
	f.tick()
	f.tick()
	require.Len(t, f.notices("s2"), 1, "the first successor is told once")
	assert.Contains(t, f.notices("s2")[0], "GM claude")
	assert.Contains(t, f.notices("s2")[0], "chat gm claim --project proj --agent codex-b --expect-term 0 --expect-gm claude")
	assert.Empty(t, f.notices("s3"))

	f.at(25 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s3"), 1, "no claim yet: the next successor is told")
	assert.Len(t, f.notices("s2"), 1)
}

func TestCohortGMAnswerClearsTheWatch(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UCL", Text: "something unrelated"})
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000300", User: "UCL", ThreadTS: "1800000000.000100", Text: "decided: yes"})
	f.at(30 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "a reply in the mention's thread answers it")
}

func TestCohortUnrelatedGMPostIsNotAnAnswer(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UCL", Text: "something unrelated"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1)
}

// The live stream can miss an answer (another home's GM, a restart); before
// telling anyone, the listener checks the thread and the GM's ✅ in Slack.
func TestCohortAnswerFoundInSlackBeforeNotifying(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	parent := msg("1800000000.000100", "UBR", "<@UCL> please decide")
	parent.Reactions = []slack.ItemReaction{{Name: "white_check_mark", Users: []string{"UCL"}}}
	f.api.replies["C1|1800000000.000100"] = []slack.Message{parent}
	f.at(15 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "the GM's ✅ (chat ack) answers it")

	f2 := newCohortFixture(t)
	f2.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	parent = msg("1800000000.000100", "UBR", "<@UCL> please decide")
	parent.Reactions = []slack.ItemReaction{{Name: "eyes", Users: []string{"UCL"}}}
	f2.api.replies["C1|1800000000.000100"] = []slack.Message{parent}
	f2.at(15 * time.Minute)
	f2.tick()
	assert.Len(t, f2.notices("s2"), 1, "a delivery receipt (👀) is not an answer")
}

func TestCohortBlockedGMIsUnavailableAtOnce(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UCL",
		Text: blockedNotice("claude", "Bash", defaultApprovalWait)})
	f.at(time.Minute)
	f.tick()
	require.Len(t, f.notices("s2"), 1)
	assert.Contains(t, f.notices("s2")[0], "BLOCKED")

	// The GM posting again means it got unstuck.
	g := newCohortFixture(t)
	g.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UCL",
		Text: blockedNotice("claude", "Bash", defaultApprovalWait)})
	g.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UCL", Text: "approved; continuing"})
	g.at(time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"))
}

func TestCohortSkipsHeldSuccessorAndHeldGM(t *testing.T) {
	f := newCohortFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(f.root, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "holds", "codex-b"), nil, 0o644))
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "a held agent is never asked to take over")
	assert.Len(t, f.notices("s3"), 1)

	g := newCohortFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(g.root, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(g.root, "proj", "holds", "claude"), nil, 0o644))
	g.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	g.at(15 * time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"), "a GM the user paused is not an outage")
}

// A notice about a former GM never evicts the new one.
func TestCohortStaleWatchDroppedOnTermChange(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"), []byte(`{"term":1,"gm":"codex-b","claim_id":"x"}`), 0o644))
	f.at(30 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"))
	assert.Empty(t, f.notices("s3"))
}

func TestCohortOffDutyAgentSkippedButStillCounted(t *testing.T) {
	f := newCohortFixture(t)
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-duty", SessionID: "s2", Text: "off", Cohort: &CohortReg{Project: "proj"}})
	require.True(t, resp.OK, resp.Error)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"))
	assert.Empty(t, f.notices("s3"), "off duty only silences this agent; codex-r's turn comes at its own step")
	f.at(25 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s3"), 1)
}

func TestCohortCheckpointNotices(t *testing.T) {
	f := newCohortFixture(t)
	f.at(119 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"))
	f.at(2 * time.Hour)
	f.tick()
	f.tick()
	require.Len(t, f.notices("s2"), 1)
	assert.Contains(t, f.notices("s2")[0], "2-hour checkpoint")
	assert.Len(t, f.notices("s3"), 1)

	f.at(2*time.Hour + 30*time.Minute)
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-checkpoint", SessionID: "s2", Cohort: &CohortReg{Project: "proj"}})
	require.True(t, resp.OK, resp.Error)
	f.at(4 * time.Hour)
	f.tick()
	assert.Len(t, f.notices("s2"), 1, "anchored to the completed checkpoint")
	assert.Len(t, f.notices("s3"), 2, "s3 never checked in: reminded at its next due point")
}

func TestCohortUnregisteredSessionGetsNothing(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	now := time.Unix(1_800_000_000, 0)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	now = now.Add(5 * time.Hour)
	l.CohortTick(context.Background())
	for _, g := range d.got {
		assert.NotContains(t, g.text, "[cohort]")
	}
}

func TestCohortLeaveAndUnsubscribeRemoveRegistration(t *testing.T) {
	f := newCohortFixture(t)
	require.True(t, f.l.Control(context.Background(), ControlRequest{Op: "cohort-leave", SessionID: "s2", Cohort: &CohortReg{Project: "proj"}}).OK)
	f.l.Unsubscribe("s3", "")
	f.at(3 * time.Hour)
	f.tick()
	assert.Empty(t, f.notices("s2"))
	assert.Empty(t, f.notices("s3"))
}

// Registrations and pending watches survive a listener restart.
func TestCohortStateSurvivesRestart(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	l2, err := NewListener(f.api, f.d, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", f.l.StateFile, f.l.Log)
	require.NoError(t, err)
	l2.Now = func() time.Time { return f.now }
	f.l = l2
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1)
}

// Review fixes (PR #6, Fable): mentions inside threads, read errors, GM
// identity, pruning, and holds changing mid-escalation.

func TestCohortMentionInsideAThreadIsAnswered(t *testing.T) {
	f := newCohortFixture(t)
	root := "1800000000.000050"
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", ThreadTS: root, User: "UBR", Text: "<@UCL> please decide"})
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", ThreadTS: root, User: "UCL", Text: "decided"})
	f.at(30 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "the GM's reply in the same thread answers a mention made in that thread")

	g := newCohortFixture(t)
	g.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", ThreadTS: root, User: "UBR", Text: "<@UCL> please decide"})
	g.api.replies["C1|"+root] = []slack.Message{msg(root, "UBR", "root"), msg("1800000000.000100", "UBR", "<@UCL> please decide"), msg("1800000000.000200", "UCL", "decided")}
	g.at(15 * time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"), "found in Slack via the thread root when the live event was missed")
}

func TestCohortGMPostMentioningSenderFoundInSlack(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.api.history["C1"] = []slack.Message{msg("1800000000.000300", "UCL", "<@UBR> decided: yes")}
	f.at(15 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"))
}

func TestCohortReadErrorKeepsRegistrations(t *testing.T) {
	f := newCohortFixture(t)
	md := filepath.Join(f.root, "proj", "PROJECT.md")
	data, _ := os.ReadFile(md)
	require.NoError(t, os.Remove(md))
	f.at(time.Minute)
	f.tick()
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-status"})
	assert.Len(t, resp.Cohort, 2, "a transiently unreadable project deregisters nobody")
	require.NoError(t, os.WriteFile(md, data, 0o644))

	require.NoError(t, os.WriteFile(md, []byte("<!-- cohort-succession: claude, codex-r -->\n"), 0o644))
	f.tick()
	resp = f.l.Control(context.Background(), ControlRequest{Op: "cohort-status"})
	require.Len(t, resp.Cohort, 1, "an agent removed from the roster is dropped")
	assert.Equal(t, "codex-r", resp.Cohort[0].Agent)
}

func TestCohortGMIdentityIsTheBotNotTheName(t *testing.T) {
	f := newCohortFixture(t)
	f.api.users["UHU"] = &slack.User{ID: "UHU", Name: "claude", Profile: slack.UserProfile{DisplayName: "claude"}}
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UHU", ThreadTS: "1800000000.000100", Text: "decided"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1, "a person named like the GM does not answer for it")

	g := newCohortFixture(t)
	g.api.users["UHU"] = &slack.User{ID: "UHU", Name: "claude", Profile: slack.UserProfile{DisplayName: "claude"}}
	g.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UHU", Text: blockedNotice("claude", "Bash", defaultApprovalWait)})
	g.at(time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"), "nor can they fake a BLOCKED")
}

func TestCohortOldWatchesArePruned(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(25 * time.Hour)
	f.tick()
	f.l.mu.Lock()
	n := len(f.l.state.GMWatches)
	f.l.mu.Unlock()
	assert.Zero(t, n)
}

func TestCohortHoldChangeMidEscalationTellsEachAgentOnce(t *testing.T) {
	f := newCohortFixture(t)
	f.l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.tick()
	require.Len(t, f.notices("s2"), 1)
	// codex-b goes on hold: the list shifts, and codex-r becomes step 0.
	require.NoError(t, os.MkdirAll(filepath.Join(f.root, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "holds", "codex-b"), nil, 0o644))
	f.at(16 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s3"), 1)
	require.NoError(t, os.Remove(filepath.Join(f.root, "proj", "holds", "codex-b")))
	f.at(17 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1, "codex-b was already told; the hold lifting does not repeat it")
}
