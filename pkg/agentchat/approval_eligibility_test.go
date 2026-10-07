package agentchat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func codexObserverListener(t *testing.T, gm string) (*Listener, *fakeSlack, *probeDeliverer, *time.Time, string) {
	t.Helper()
	api := newFakeSlack()
	d := &probeDeliverer{}
	root := t.TempDir()
	order := "claude, codex-b"
	if gm == "codex-b" {
		order = "codex-b, claude"
	}
	writeProject(t, root, "proj", "<!-- cohort-succession: "+order+" -->\n")
	now := time.Unix(1_800_000_000, 0)
	l, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), &Subscription{SessionID: "th1", Kind: KindCodex, ThreadID: "th1", Channels: []string{"C1"}}, 0))
	require.True(t, l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "th1", Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: root, Channel: "C1"}}).OK)
	return l, api, d, &now, root
}

func TestCohortCodexObserverSkipsIneligible(t *testing.T) {
	for _, kind := range []string{"off-duty", "held", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			l, api, d, now, root := codexObserverListener(t, "claude")
			d.set("th1", true)
			l.CohortTick(context.Background())
			switch kind {
			case "off-duty":
				require.True(t, l.Control(context.Background(), ControlRequest{Op: "cohort-duty", SessionID: "th1", Text: "off", Cohort: &CohortReg{Project: "proj"}}).OK)
			case "held":
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proj", "holds"), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "proj", "holds", "codex-b"), nil, 0600))
			case "unreadable":
				require.NoError(t, os.Remove(filepath.Join(root, "proj", "PROJECT.md")))
			}
			*now = now.Add(10 * time.Minute)
			l.CohortTick(context.Background())
			require.Empty(t, api.posts(), "ineligible agent must not be broadcast as BLOCKED")
		})
	}
}

func TestCohortCodexClearResolvesBlockedWatch(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1)
	l.trackCohort(context.Background(), Message{Channel: "C1", User: "UCB", Text: blockedNotice("codex-b", "", defaultApprovalWait), TS: NowTS(*now)})
	require.Len(t, l.state.GMWatches, 1)
	d.set("th1", false)
	*now = now.Add(time.Minute)
	l.CohortTick(context.Background())
	require.Empty(t, l.state.GMWatches, "known cleared approval wait must no longer authorize a BLOCKED takeover")
}

// A failed read after a posted BLOCKED never asserts recovery; the next
// known clear does.
func TestCohortCodexRecoveryNeedsAKnownClear(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1)
	d.errs = map[string]error{"th1": ErrCodexUnavailable}
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1, "unknown is not recovered")
	d.errs = nil
	d.set("th1", false)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 2)
	require.Equal(t, "C1|"+unblockedNotice("codex-b"), api.posts()[1])
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 2, "announced once")
}

// The tick that observes recovery does not also deliver an escalation for
// the BLOCKED watch it just retired.
func TestCohortCodexRecoveryStopsSameTickEscalation(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	require.NoError(t, l.Subscribe(context.Background(), &Subscription{SessionID: "s9", Kind: KindCodex, ThreadID: "s9", Channels: []string{"C1"}}, 0))
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1)
	l.trackCohort(context.Background(), Message{Channel: "C1", User: "UCB", Text: blockedNotice("codex-b", "", defaultApprovalWait), TS: NowTS(*now)})
	require.Len(t, l.state.GMWatches, 1)
	// The successor's slot comes due in the same tick the clear is seen.
	*now = now.Add(15 * time.Minute)
	d.set("th1", false)
	before := len(d.got)
	l.CohortTick(context.Background())
	require.Empty(t, l.state.GMWatches)
	for _, g := range d.got[before:] {
		require.NotContains(t, g.text, "BLOCKED", "no escalation for a watch retired this tick")
	}
}

// Recovery survives a listener restart: the durable BLOCKED watch stands for
// the posted notice. A session still waiting after the restart is not
// announced again.
func TestCohortCodexRecoverySurvivesRestart(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1)
	l.trackCohort(context.Background(), Message{Channel: "C1", User: "UCB", Text: blockedNotice("codex-b", "", defaultApprovalWait), TS: NowTS(*now)})
	require.Len(t, l.state.GMWatches, 1)

	restarted, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", l.StateFile, l.Log)
	require.NoError(t, err)
	restarted.Now = func() time.Time { return *now }
	*now = now.Add(20 * time.Minute)
	restarted.CohortTick(context.Background())
	require.Len(t, api.posts(), 1, "still waiting after the restart: not announced again")

	d.set("th1", false)
	*now = now.Add(time.Minute)
	restarted.CohortTick(context.Background())
	require.Empty(t, restarted.state.GMWatches, "known recovery retires the durable watch")
	require.Len(t, api.posts(), 2)
	require.Equal(t, "C1|"+unblockedNotice("codex-b"), api.posts()[1])
}

// A crash after Slack accepts BLOCKED, before this listener sees its own
// event, still leaves the recovery owed.
func TestCohortCodexRecoveryOwedBeforeOwnSlackEvent(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1, "Slack accepted BLOCKED; peers can now observe it")
	// Crash after PostMessage returned but before this listener receives its own event.
	restarted, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", l.StateFile, l.Log)
	require.NoError(t, err)
	restarted.Now = func() time.Time { return *now }
	d.set("th1", false)
	*now = now.Add(time.Minute)
	restarted.CohortTick(context.Background())
	require.Len(t, api.posts(), 2, "accepted BLOCKED needs recovery even before own event was saved")
	require.Equal(t, "C1|"+unblockedNotice("codex-b"), api.posts()[1])
}

// A BLOCKED post whose result is uncertain (an error after Slack may have
// accepted it) still owes recovery on the next known clear.
func TestCohortCodexUncertainBlockedPostOwesRecovery(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	api.failPosts = true
	l.CohortTick(context.Background())
	api.failPosts = false
	require.Empty(t, api.posts())
	d.set("th1", false)
	l.CohortTick(context.Background())
	require.Equal(t, []string{"C1|" + unblockedNotice("codex-b")}, api.posts(), "recovery on the first known clear")
	l.CohortTick(context.Background())
	require.Len(t, api.posts(), 1, "and only once")
}

// An announcement record alone is not proof of delivery: after a restart a
// session still waiting is announced again after a fresh observed count.
func TestCohortCodexAnnouncementIntentRetriesAfterRestart(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	api.failPosts = true
	l.CohortTick(context.Background())
	require.Empty(t, api.posts(), "no BLOCKED notice reached Slack")
	require.True(t, l.state.Announced[cohortKey("th1", "proj")])
	restarted, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", l.StateFile, l.Log)
	require.NoError(t, err)
	restarted.Now = func() time.Time { return *now }
	api.failPosts = false
	restarted.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	restarted.CohortTick(context.Background())
	require.NotEmpty(t, api.posts(), "an uncertain intent must not suppress BLOCKED forever while the thread keeps waiting")
}

// A failed read restarts the observed count even while recovery is owed.
func TestCohortCodexOwedWaitRestartsCountOnProbeError(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	api.failPosts = true
	l.CohortTick(context.Background()) // owed through the announcement record
	api.failPosts = false
	require.Empty(t, api.posts())
	*now = now.Add(5 * time.Minute)
	d.mu.Lock()
	d.errs = map[string]error{"th1": ErrCodexUnavailable}
	d.mu.Unlock()
	l.CohortTick(context.Background())
	d.mu.Lock()
	d.errs = nil
	d.mu.Unlock()
	*now = now.Add(9 * time.Minute)
	l.CohortTick(context.Background())
	require.Empty(t, api.posts(), "9 minutes observed since the error: not yet")
	*now = now.Add(time.Minute)
	l.CohortTick(context.Background())
	require.Equal(t, []string{"C1|" + blockedNotice("codex-b", "", defaultApprovalWait)}, api.posts())
}

// An errored post that Slack did accept shows up as our own watch; that
// counts as delivery, so no duplicate is posted.
func TestCohortCodexOwnWatchCountsAsDelivery(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	api.failPosts = true
	l.CohortTick(context.Background())
	api.failPosts = false
	l.trackCohort(context.Background(), Message{Channel: "C1", User: "UCB", Text: blockedNotice("codex-b", "", defaultApprovalWait), TS: NowTS(*now)})
	*now = now.Add(time.Minute)
	l.CohortTick(context.Background())
	require.Empty(t, api.posts(), "no duplicate after the accepted notice")
	d.set("th1", false)
	l.CohortTick(context.Background())
	require.Equal(t, []string{"C1|" + unblockedNotice("codex-b")}, api.posts())
}

// After a restart, an unconfirmed announcement's retry count restarts on an
// unknown read.
func TestCohortCodexUnconfirmedRetryCountResetsOnUnknown(t *testing.T) {
	l, api, d, now, _ := codexObserverListener(t, "codex-b")
	d.set("th1", true)
	l.CohortTick(context.Background())
	*now = now.Add(10 * time.Minute)
	api.failPosts = true
	l.CohortTick(context.Background())
	restarted, err := NewListener(api, d, Identity{UserID: "UCB", BotID: "BCB"}, "UBR", l.StateFile, l.Log)
	require.NoError(t, err)
	restarted.Now = func() time.Time { return *now }
	api.failPosts = false
	restarted.CohortTick(context.Background())
	*now = now.Add(8 * time.Minute)
	d.errs = map[string]error{"th1": errors.New("unknown observation")}
	restarted.CohortTick(context.Background())
	d.errs = nil
	*now = now.Add(3 * time.Minute)
	restarted.CohortTick(context.Background())
	require.Empty(t, api.posts(), "recovery owed must not turn an unknown gap into continuous observed waiting")
	require.True(t, restarted.state.Announced[cohortKey("th1", "proj")], "recovery intent stays durable while the count restarts")
}
