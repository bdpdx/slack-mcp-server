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

// Review fixes (PR #6, Fable): mentions inside threads, read errors, GM
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
