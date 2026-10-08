package agentchat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// cohortFixture is two homes, one per agent as in real use: listener l
// serves codex-b (session s2) and l3 serves codex-r (session s3). Both watch
// project channel C1 ("proj"), whose GM is claude.
type cohortFixture struct {
	t    *testing.T
	api  *fakeSlack
	d    *fakeDeliverer
	l    *Listener // codex-b's home
	l3   *Listener // codex-r's home
	root string
	now  time.Time
}

func newCohortFixture(t *testing.T) *cohortFixture {
	f := &cohortFixture{t: t, api: newFakeSlack(), d: &fakeDeliverer{}, root: t.TempDir(), now: time.Unix(1_800_000_000, 0)}
	writeProject(t, f.root, "proj", "<!-- cohort-succession: claude, codex-b, codex-r -->\n")
	f.l = newTestListenerAs(t, f.api, f.d, Identity{UserID: "UCB", BotID: "BCB"})
	f.l3 = newTestListenerAs(t, f.api, f.d, Identity{UserID: "UCR", BotID: "BCR"})
	for _, h := range []struct {
		l              *Listener
		session, agent string
	}{{f.l, "s2", "codex-b"}, {f.l3, "s3", "codex-r"}} {
		h.l.Now = func() time.Time { return f.now }
		require.NoError(t, h.l.Subscribe(context.Background(), claudeSub(h.session), 0))
		resp := h.l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: h.session,
			Cohort: &CohortReg{Project: "proj", Agent: h.agent, Root: f.root, Channel: "C1"}})
		require.True(t, resp.OK, resp.Error)
	}
	return f
}

// at sets the clock to d after the base time, plus a second so deadlines
// counted from messages stamped just after the base have passed.
func (f *cohortFixture) at(d time.Duration) { f.now = time.Unix(1_800_000_000, 0).Add(d + time.Second) }

func (f *cohortFixture) homes() []*Listener { return []*Listener{f.l, f.l3} }

// handle delivers a Slack message to both homes, as Socket Mode would.
func (f *cohortFixture) handle(_ context.Context, m Message) {
	for _, l := range f.homes() {
		l.HandleMessage(context.Background(), m)
	}
}

func (f *cohortFixture) tick() {
	for _, l := range f.homes() {
		l.CohortTick(context.Background())
	}
}

// status merges both homes' registrations.
func (f *cohortFixture) status() []CohortReg {
	var out []CohortReg
	for _, l := range f.homes() {
		out = append(out, l.Control(context.Background(), ControlRequest{Op: "cohort-status"}).Cohort...)
	}
	return out
}

func (f *cohortFixture) watchCount() int {
	n := 0
	for _, l := range f.homes() {
		l.mu.Lock()
		n += len(l.state.GMWatches)
		l.mu.Unlock()
	}
	return n
}

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
	assert.Len(t, f.status(), 2)
}

// An @mention of the GM left unanswered for 15 minutes tells the first
// successor, then 10 minutes later the next one.
func TestCohortUnansweredMentionEscalates(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})

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
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UCL", Text: "something unrelated"})
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000300", User: "UCL", ThreadTS: "1800000000.000100", Text: "decided: yes"})
	f.at(30 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "a reply in the mention's thread answers it")
}

func TestCohortUnrelatedGMPostIsNotAnAnswer(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UCL", Text: "something unrelated"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1)
}

// The live stream can miss an answer (another home's GM, a restart); before
// telling anyone, the listener checks the thread and the GM's ✅ in Slack.
func TestCohortAnswerFoundInSlackBeforeNotifying(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	parent := msg("1800000000.000100", "UBR", "<@UCL> please decide")
	parent.Reactions = []slack.ItemReaction{{Name: "white_check_mark", Users: []string{"UCL"}}}
	f.api.replies["C1|1800000000.000100"] = []slack.Message{parent}
	f.at(15 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "the GM's ✅ (chat ack) answers it")

	f2 := newCohortFixture(t)
	f2.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	parent = msg("1800000000.000100", "UBR", "<@UCL> please decide")
	parent.Reactions = []slack.ItemReaction{{Name: "eyes", Users: []string{"UCL"}}}
	f2.api.replies["C1|1800000000.000100"] = []slack.Message{parent}
	f2.at(15 * time.Minute)
	f2.tick()
	assert.Len(t, f2.notices("s2"), 1, "a delivery receipt (👀) is not an answer")
}

func TestCohortBlockedGMIsUnavailableAtOnce(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UCL",
		Text: blockedNotice("claude", "Bash", defaultApprovalWait)})
	f.at(time.Minute)
	f.tick()
	require.Len(t, f.notices("s2"), 1)
	assert.Contains(t, f.notices("s2")[0], "BLOCKED")

	// The GM posting again means it got unstuck.
	g := newCohortFixture(t)
	g.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UCL",
		Text: blockedNotice("claude", "Bash", defaultApprovalWait)})
	g.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UCL", Text: "approved; continuing"})
	g.at(time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"))
}

func TestCohortSkipsHeldSuccessorAndHeldGM(t *testing.T) {
	f := newCohortFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(f.root, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "holds", "codex-b"), nil, 0o644))
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "a held agent is never asked to take over")
	assert.Len(t, f.notices("s3"), 1)

	g := newCohortFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(g.root, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(g.root, "proj", "holds", "claude"), nil, 0o644))
	g.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	g.at(15 * time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"), "a GM the user paused is not an outage")
}

// A notice about a former GM never evicts the new one.
func TestCohortStaleWatchDroppedOnTermChange(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
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
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
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
	f.l3.Unsubscribe("s3", "")
	f.at(3 * time.Hour)
	f.tick()
	assert.Empty(t, f.notices("s2"))
	assert.Empty(t, f.notices("s3"))
}

// Registrations and pending watches survive a listener restart.
func TestCohortStateSurvivesRestart(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	l2, err := NewListener(f.api, f.d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", f.l.StateFile, f.l.Log)
	require.NoError(t, err)
	l2.Now = func() time.Time { return f.now }
	f.l = l2
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1)
}

// Review fixes: mentions inside threads, read errors, GM
// identity, pruning, and holds changing mid-escalation.

func TestCohortMentionInsideAThreadIsAnswered(t *testing.T) {
	f := newCohortFixture(t)
	root := "1800000000.000050"
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", ThreadTS: root, User: "UBR", Text: "<@UCL> please decide"})
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", ThreadTS: root, User: "UCL", Text: "decided"})
	f.at(30 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"), "the GM's reply in the same thread answers a mention made in that thread")

	g := newCohortFixture(t)
	g.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", ThreadTS: root, User: "UBR", Text: "<@UCL> please decide"})
	g.api.replies["C1|"+root] = []slack.Message{msg(root, "UBR", "root"), msg("1800000000.000100", "UBR", "<@UCL> please decide"), msg("1800000000.000200", "UCL", "decided")}
	g.at(15 * time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"), "found in Slack via the thread root when the live event was missed")
}

func TestCohortGMPostMentioningSenderFoundInSlack(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
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
	assert.Len(t, f.status(), 2, "a transiently unreadable project deregisters nobody")
	require.NoError(t, os.WriteFile(md, data, 0o644))

	require.NoError(t, os.WriteFile(md, []byte("<!-- cohort-succession: claude, codex-r -->\n"), 0o644))
	f.tick()
	st := f.status()
	require.Len(t, st, 1, "an agent removed from the roster is dropped")
	assert.Equal(t, "codex-r", st[0].Agent)
}

func TestCohortGMIdentityIsTheBotNotTheName(t *testing.T) {
	f := newCohortFixture(t)
	f.api.users["UHU"] = &slack.User{ID: "UHU", Name: "claude", Profile: slack.UserProfile{DisplayName: "claude"}}
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000200", User: "UHU", ThreadTS: "1800000000.000100", Text: "decided"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1, "a person named like the GM does not answer for it")

	g := newCohortFixture(t)
	g.api.users["UHU"] = &slack.User{ID: "UHU", Name: "claude", Profile: slack.UserProfile{DisplayName: "claude"}}
	g.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UHU", Text: blockedNotice("claude", "Bash", defaultApprovalWait)})
	g.at(time.Minute)
	g.tick()
	assert.Empty(t, g.notices("s2"), "nor can they fake a BLOCKED")
}

func TestCohortOldWatchesArePruned(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(25 * time.Hour)
	f.tick()
	assert.Zero(t, f.watchCount())
}

func TestCohortHoldChangeMidEscalationTellsEachAgentOnce(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
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

func TestCohortHeldAgentGetsNoCheckpoint(t *testing.T) {
	f := newCohortFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(f.root, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "holds", "codex-b"), nil, 0o644))
	f.at(2 * time.Hour)
	f.tick()
	assert.Empty(t, f.notices("s2"), "a user's hold silences checkpoint wakeups")
	assert.Len(t, f.notices("s3"), 1)

	g := newCohortFixture(t)
	require.NoError(t, os.Remove(filepath.Join(g.root, "proj", "PROJECT.md")))
	g.at(2 * time.Hour)
	g.tick()
	assert.Empty(t, g.notices("s2"), "unreadable authority: fail closed")
}

// A home registers only its own agent.
func TestCohortRegisterRefusesAnotherAgentsName(t *testing.T) {
	f := newCohortFixture(t)
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "s2",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-r", Root: f.root, Channel: "C1"}})
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "this home's agent is codex-b")
}

func TestCohortStatusReportsPendingDeadlines(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	resp := f.l.Control(context.Background(), ControlRequest{Op: "cohort-status", Cohort: &CohortReg{Project: "proj"}})
	require.Len(t, resp.Watches, 1)
	assert.Equal(t, "claude", resp.Watches[0].GM)
}

// A mention seen late (a reconnect, a backlog) keeps its original deadline.
func TestCohortDeadlineCountsFromTheMessageNotReceipt(t *testing.T) {
	f := newCohortFixture(t)
	f.at(14 * time.Minute) // the listener only sees the message now
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	f.tick()
	assert.Len(t, f.notices("s2"), 1, "15 minutes from the message, not from receipt")
}

func TestCohortRegisterFailsClosedWithoutIdentity(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListenerAs(t, api, d, Identity{UserID: "UNOBODY", BotID: "BX"})
	root := t.TempDir()
	writeProject(t, root, "proj", "<!-- cohort-succession: claude, codex-b -->\n")
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	resp := l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "s2",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: root, Channel: "C1"}})
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "cannot resolve")
}

func TestCohortConcurrentRegistrations(t *testing.T) {
	f := newCohortFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "s2",
				Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: f.root, Channel: "C1"}})
		}()
	}
	wg.Wait()
	assert.Len(t, f.status(), 2)
}

func TestCohortClaimCheck(t *testing.T) {
	f := newCohortFixture(t)
	check := func(l *Listener, session, agent string, term int, gm string) ControlResponse {
		return l.Control(context.Background(), ControlRequest{Op: "cohort-claim-check", SessionID: session,
			Cohort: &CohortReg{Project: "proj", Agent: agent}, Expect: &GMState{Term: term, GM: gm}})
	}
	assert.False(t, check(f.l, "s2", "codex-b", 0, "claude").OK, "no deadline yet")
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	f.at(15 * time.Minute)
	assert.True(t, check(f.l, "s2", "codex-b", 0, "claude").OK, "codex-b's slot (step 1) has come")
	assert.False(t, check(f.l3, "s3", "codex-r", 0, "claude").OK, "codex-r's slot has not")
	assert.False(t, check(f.l, "s2", "codex-b", 3, "claude").OK, "wrong expected term")
	assert.False(t, check(f.l, "s2", "codex-r", 0, "claude").OK, "registered as another agent")
	f.at(25 * time.Minute)
	assert.True(t, check(f.l3, "s3", "codex-r", 0, "claude").OK)
	assert.True(t, check(f.l, "s2", "codex-b", 0, "claude").OK, "an earlier successor stays eligible")
	require.True(t, f.l.Control(context.Background(), ControlRequest{Op: "cohort-duty", SessionID: "s2", Text: "off", Cohort: &CohortReg{Project: "proj"}}).OK)
	assert.False(t, check(f.l, "s2", "codex-b", 0, "claude").OK, "off duty")
}

// probeDeliverer is a fakeDeliverer whose sessions can report waiting on an
// approval, as Codex threads do through thread/read.
type probeDeliverer struct {
	fakeDeliverer
	mu      sync.Mutex
	waiting map[string]bool
	errs    map[string]error
}

func (p *probeDeliverer) WaitingOnApproval(_ context.Context, sub *Subscription) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiting[sub.SessionID], p.errs[sub.SessionID]
}

func (p *probeDeliverer) set(session string, waiting bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		p.waiting = map[string]bool{}
	}
	p.waiting[session] = waiting
}

func TestCohortCodexApprovalWaitPostsBlocked(t *testing.T) {
	api := newFakeSlack()
	d := &probeDeliverer{}
	root := t.TempDir()
	writeProject(t, root, "proj", "<!-- cohort-succession: claude, codex-b -->\n")
	now := time.Unix(1_800_000_000, 0)
	l, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Now = func() time.Time { return now }
	sub := &Subscription{SessionID: "th1", Kind: KindCodex, ThreadID: "th1", Channels: []string{"C1"}}
	require.NoError(t, l.Subscribe(context.Background(), sub, 0))
	resp := l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "th1",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: root, Channel: "C1"}})
	require.True(t, resp.OK, resp.Error)

	d.set("th1", true)
	l.CohortTick(context.Background())
	now = now.Add(9 * time.Minute)
	l.CohortTick(context.Background())
	assert.Empty(t, api.posts(), "not yet 10 minutes")
	now = now.Add(time.Minute)
	l.CohortTick(context.Background())
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1, "posted once")
	assert.Equal(t, "C1|"+blockedNotice("codex-b", "", defaultApprovalWait), api.posts()[0])

	// The flag clears, then a new wait starts its own 10 minutes.
	d.set("th1", false)
	l.CohortTick(context.Background())
	d.set("th1", true)
	now = now.Add(time.Minute)
	l.CohortTick(context.Background())
	now = now.Add(5 * time.Minute)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 2, "the known clear was announced once; the new wait has not reached 10 minutes")
	assert.Equal(t, "C1|"+unblockedNotice("codex-b"), api.posts()[1])
}

func TestCohortCodexProbeFailureResetsTheWait(t *testing.T) {
	api := newFakeSlack()
	d := &probeDeliverer{errs: map[string]error{}}
	root := t.TempDir()
	writeProject(t, root, "proj", "<!-- cohort-succession: claude, codex-b -->\n")
	now := time.Unix(1_800_000_000, 0)
	l, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), &Subscription{SessionID: "th1", Kind: KindCodex, ThreadID: "th1", Channels: []string{"C1"}}, 0))
	require.True(t, l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "th1",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: root, Channel: "C1"}}).OK)
	d.set("th1", true)
	l.CohortTick(context.Background())
	now = now.Add(8 * time.Minute)
	d.errs["th1"] = ErrCodexUnavailable
	l.CohortTick(context.Background())
	delete(d.errs, "th1")
	now = now.Add(3 * time.Minute)
	l.CohortTick(context.Background())
	assert.Empty(t, api.posts(), "an unobservable gap restarts the count: never post on a guess")
}

func TestCohortCodexWaitIsPerRegistration(t *testing.T) {
	api := newFakeSlack()
	d := &probeDeliverer{}
	root := t.TempDir()
	writeProject(t, root, "proj", "<!-- cohort-succession: claude, codex-b -->\n")
	writeProject(t, root, "other", "<!-- cohort-succession: claude, codex-b -->\n")
	now := time.Unix(1_800_000_000, 0)
	l, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), &Subscription{SessionID: "th1", Kind: KindCodex, ThreadID: "th1", Channels: []string{"C1", "C2"}}, 0))
	register := func(project, channel string) {
		resp := l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "th1",
			Cohort: &CohortReg{Project: project, Agent: "codex-b", Root: root, Channel: channel}})
		require.True(t, resp.OK, resp.Error)
	}
	register("proj", "C1")
	register("other", "C2")

	d.set("th1", true)
	l.CohortTick(context.Background())
	now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	assert.ElementsMatch(t, []string{
		"C1|" + blockedNotice("codex-b", "", defaultApprovalWait),
		"C2|" + blockedNotice("codex-b", "", defaultApprovalWait),
	}, api.posts(), "each project channel hears")

	// Leaving drops the wait; re-registering while still waiting starts a
	// fresh count and posts again.
	require.True(t, l.Control(context.Background(), ControlRequest{Op: "cohort-leave", SessionID: "th1", Cohort: &CohortReg{Project: "other"}}).OK)
	l.CohortTick(context.Background())
	register("other", "C2")
	l.CohortTick(context.Background())
	now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	assert.Len(t, api.posts(), 3)
	assert.Equal(t, "C2|"+blockedNotice("codex-b", "", defaultApprovalWait), api.posts()[2])
}

// A mention of a new GM arriving while each home's cached view still names
// the former one is classified against a re-read view, so the single
// unanswered mention still produces its successor notice.
func TestCohortNewGMMentionSurvivesCachedFormerAuthority(t *testing.T) {
	f := newCohortFixture(t)
	// Registration primed each home's view with the original term-0 GM.
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"),
		[]byte(`{"term":1,"gm":"codex-r","claim_id":"changed-authority"}`), 0o600))
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100",
		User: "UBR", Text: "<@UCR> please decide"})
	f.at(26 * time.Minute)
	f.tick()
	f.tick()
	require.Len(t, f.notices("s2"), 1, "the mention survives the stale view")
}

// A BLOCKED notice from a new GM the cached view does not yet name is still
// an outage.
func TestCohortNewGMBlockedSurvivesCachedFormerAuthority(t *testing.T) {
	f := newCohortFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"),
		[]byte(`{"term":1,"gm":"codex-r","claim_id":"changed-authority"}`), 0o600))
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100",
		User: "UCR", Text: "BLOCKED: codex-r has waited 10m0s for approval of Bash; it is now waiting at the terminal."})
	f.at(11 * time.Minute)
	f.tick()
	require.Len(t, f.notices("s2"), 1, "the next live successor is told")
}

// Ordinary chatter that names no other roster agent does not force a re-read.
func TestCohortRecheckOnlyForPossibleGMSignals(t *testing.T) {
	f := newCohortFixture(t)
	p, err := f.l.projectView(context.Background(), f.root, "proj", false)
	require.NoError(t, err)
	assert.False(t, f.l.mayNameUncachedGM(context.Background(), Message{User: "UCR", Text: "status update"}, p))
	assert.False(t, f.l.mayNameUncachedGM(context.Background(), Message{User: "UBR", Text: "<@UCL> please decide"}, p), "the cached GM")
	assert.True(t, f.l.mayNameUncachedGM(context.Background(), Message{User: "UBR", Text: "<@UCR> please decide"}, p))
	assert.True(t, f.l.mayNameUncachedGM(context.Background(), Message{User: "UCR", Text: "BLOCKED: codex-r has waited"}, p))
	assert.False(t, f.l.mayNameUncachedGM(context.Background(), Message{User: "UCR", Text: "<@UCR> self"}, p), "a self-mention")
}

// When the new authority is not yet visible on arrival (a push still
// landing, or from another machine before the next fetch), the mention is
// kept and classified on a later tick, still counting from its own time. A
// second, non-GM mention in the same message does not hold it back.
func TestCohortNewGMMentionReplayedAfterAuthorityLands(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100",
		User: "UBR", Text: "<@UCR> <@UCB> please decide"})
	assert.Equal(t, 0, f.watchCount(), "codex-r is not GM in any view yet")
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"),
		[]byte(`{"term":1,"gm":"codex-r","claim_id":"changed-authority"}`), 0o600))
	f.at(2 * time.Minute)
	f.tick() // the refreshed view now names codex-r; the kept mention is classified
	require.Equal(t, 2, f.watchCount(), "one watch per home")
	f.at(26 * time.Minute)
	f.tick()
	f.tick()
	require.Len(t, f.notices("s2"), 1, "deadlines count from the mention, not the replay")
}

// A kept mention the new GM already answered before its authority became
// visible must not raise a false outage when it is replayed.
func TestCohortReplayedMentionAlreadyAnsweredIsDropped(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100",
		User: "UBR", Text: "<@UCR> please decide"})
	reply := slack.Message{Msg: slack.Msg{User: "UCR", Timestamp: "1800000010.000100", ThreadTimestamp: "1800000000.000100", Text: "on it"}}
	parent := slack.Message{Msg: slack.Msg{User: "UBR", Timestamp: "1800000000.000100", Text: "<@UCR> please decide"}}
	f.api.mu.Lock()
	f.api.replies["C1|1800000000.000100"] = []slack.Message{parent, reply}
	f.api.mu.Unlock()
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000010.000100", ThreadTS: "1800000000.000100", User: "UCR", Text: "on it"})
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"),
		[]byte(`{"term":1,"gm":"codex-r","claim_id":"changed-authority"}`), 0o600))
	f.at(2 * time.Minute)
	f.tick()
	assert.Equal(t, 0, f.watchCount(), "the answer predates the replay")
	f.at(26 * time.Minute)
	f.tick()
	f.tick()
	assert.Empty(t, f.notices("s2"))
}

// With a real remote: a claim published from another clone one second after
// this home's fetch is invisible to its local re-read, so the sole mention of
// the new GM is kept and classified once the routine fetch sees the claim.
func TestCohortNewGMMentionAfterRemoteClaimWithinFetchInterval(t *testing.T) {
	_, clones := gmRepo(t, 2)
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListenerAs(t, api, d, Identity{UserID: "UCB", BotID: "BCB"})
	now := time.Unix(1_800_000_000, 0)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	resp := l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "s2",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: clones[0], Channel: "C1"}})
	require.True(t, resp.OK, resp.Error)
	r, err := newClaimer(clones[1], "codex-r").Claim(context.Background(), 0, "claude", "elsewhere")
	require.NoError(t, err)
	require.Equal(t, ClaimWon, r.Outcome)
	now = now.Add(time.Second)
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000001.000100", User: "UBR", Text: "<@UCR> please decide"})
	now = now.Add(26 * time.Minute)
	l.CohortTick(context.Background())
	n := 0
	for _, g := range d.got {
		if strings.Contains(g.text, "[cohort]") {
			n++
		}
	}
	require.Equal(t, 1, n, "the mention survives the fetch interval")
}

// A mention of an agent from before it became GM owed no GM answer: once it
// claims, that kept message is not replayed into an outage.
func TestCohortPreClaimMentionIsNotReplayed(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100",
		User: "UBR", Text: "<@UCR> heads up, rez1 is down"})
	f.at(5 * time.Minute)
	since := f.now.Format(time.RFC3339)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"),
		[]byte(`{"term":1,"gm":"codex-r","claim_id":"later","since":"`+since+`"}`), 0o600))
	f.at(6 * time.Minute)
	f.tick()
	f.tick()
	assert.Equal(t, 0, f.watchCount(), "sent before codex-r became GM")
	f.at(29 * time.Minute)
	f.tick()
	assert.Empty(t, f.notices("s2"))
	assert.Empty(t, f.notices("s3"))
}

// With a real remote, a joint ask naming the new GM beside another roster
// agent is replayed like a single mention.
func TestCohortNewGMMixedMentionAfterRemoteClaim(t *testing.T) {
	_, clones := gmRepo(t, 2)
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListenerAs(t, api, d, Identity{UserID: "UCB", BotID: "BCB"})
	now := time.Unix(1_800_000_000, 0)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	resp := l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "s2",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: clones[0], Channel: "C1"}})
	require.True(t, resp.OK, resp.Error)
	r, err := newClaimer(clones[1], "codex-r").Claim(context.Background(), 0, "claude", "elsewhere")
	require.NoError(t, err)
	require.Equal(t, ClaimWon, r.Outcome)
	now = now.Add(time.Second)
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000001.000100", User: "UBR", Text: "<@UCR> <@UCB> please decide"})
	now = now.Add(26 * time.Minute)
	l.CohortTick(context.Background())
	n := 0
	for _, g := range d.got {
		if strings.Contains(g.text, "[cohort]") {
			n++
		}
	}
	require.Equal(t, 1, n, "the extra non-GM mention does not suppress the GM watch")
}

// A replayed joint ask is not kept again: after the new GM answers it live,
// later ticks neither recreate the watch nor, if Slack reads fail, raise a
// false outage for it.
func TestCohortReplayedJointAskIsNotKeptAgain(t *testing.T) {
	f := newCohortFixture(t)
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000000.000100",
		User: "UBR", Text: "<@UCR> <@UCB> please decide"})
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "proj", "gm.json"),
		[]byte(`{"term":1,"gm":"codex-r","claim_id":"changed-authority"}`), 0o600))
	f.at(2 * time.Minute)
	f.tick()
	require.Equal(t, 2, f.watchCount())
	f.handle(context.Background(), Message{Channel: "C1", TS: "1800000150.000100", ThreadTS: "1800000000.000100", User: "UCR", Text: "on it"})
	require.Equal(t, 0, f.watchCount(), "answered live")
	for _, l := range f.homes() {
		l.mu.Lock()
		kept := len(l.state.Rechecks)
		l.mu.Unlock()
		assert.Equal(t, 0, kept, "nothing left to replay")
	}
	f.api.mu.Lock()
	f.api.failReads = true
	f.api.mu.Unlock()
	for _, d := range []time.Duration{3, 8, 26} {
		f.at(d * time.Minute)
		f.tick()
	}
	assert.Equal(t, 0, f.watchCount())
	assert.Empty(t, f.notices("s2"))
}
