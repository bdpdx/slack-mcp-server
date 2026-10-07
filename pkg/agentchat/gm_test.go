package agentchat

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// gmRepo is a bare "GitHub" remote holding one project, and n clones of it.
func gmRepo(t *testing.T, n int) (remote string, clones []string) {
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", remote)
	seed := filepath.Join(base, "seed")
	gitT(t, base, "clone", "-q", remote, seed)
	writeProject(t, seed, "proj", "<!-- cohort-succession: claude, codex-b, codex-r -->\n")
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-qm", "seed")
	gitT(t, seed, "push", "-q", "origin", "HEAD:main")
	for i := 0; i < n; i++ {
		c := filepath.Join(base, "clone"+string(rune('a'+i)))
		gitT(t, base, "clone", "-q", remote, c)
		clones = append(clones, c)
	}
	return remote, clones
}

func remoteGM(t *testing.T, remote string) GMState {
	t.Helper()
	out := gitT(t, remote, "show", "main:proj/gm.json")
	var g GMState
	require.NoError(t, json.Unmarshal([]byte(out), &g))
	return g
}

func newClaimer(root, agent string) *GMAuthority {
	return &GMAuthority{Root: root, Project: "proj", Agent: agent, Env: []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}}
}

func TestGMStatusDefaultsToFirstInOrder(t *testing.T) {
	_, clones := gmRepo(t, 1)
	st, err := newClaimer(clones[0], "codex-b").Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, GMState{Term: 0, GM: "claude"}, st)
}

func TestGMClaimWinsAndIsReceipted(t *testing.T) {
	remote, clones := gmRepo(t, 1)
	before := gitT(t, clones[0], "rev-parse", "HEAD")
	r, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "GM unanswered 15m")
	require.NoError(t, err)
	assert.Equal(t, ClaimWon, r.Outcome)
	assert.Equal(t, 1, r.State.Term)
	assert.Equal(t, "codex-b", r.State.GM)
	assert.NotEmpty(t, r.State.ClaimID)
	assert.Equal(t, r.State, remoteGM(t, remote))
	assert.Equal(t, before, gitT(t, clones[0], "rev-parse", "HEAD"), "the shared working branch is never touched")
	assert.Empty(t, gitT(t, clones[0], "status", "--porcelain"))
}

func TestGMClaimRefusesStaleExpectation(t *testing.T) {
	_, clones := gmRepo(t, 2)
	_, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	r, err := newClaimer(clones[1], "codex-r").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimLost, r.Outcome, "the term moved on: a stale notice cannot evict the new GM")
	assert.Equal(t, "codex-b", r.State.GM)
}

// The descendant race: two homes sharing one clone, or racing from the same
// captured tip, must leave exactly one winner, and the loser must know.
func TestGMClaimRaceHasExactlyOneWinner(t *testing.T) {
	for _, shared := range []bool{true, false} {
		remote, clones := gmRepo(t, 2)
		a, b := clones[0], clones[1]
		if shared {
			b = a
		}
		var wg sync.WaitGroup
		results := make([]ClaimResult, 2)
		errs := make([]error, 2)
		for i, c := range []*GMAuthority{newClaimer(a, "codex-b"), newClaimer(b, "codex-r")} {
			wg.Add(1)
			go func(i int, c *GMAuthority) {
				defer wg.Done()
				results[i], errs[i] = c.Claim(context.Background(), 0, "claude", "race")
			}(i, c)
		}
		wg.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		won := 0
		for _, r := range results {
			if r.Outcome == ClaimWon {
				won++
			}
		}
		assert.Equal(t, 1, won, "shared=%v: exactly one winner", shared)
		g := remoteGM(t, remote)
		assert.Equal(t, 1, g.Term)
		for _, r := range results {
			if r.Outcome == ClaimWon {
				assert.Equal(t, g.ClaimID, r.State.ClaimID, "the winner is the one on the remote")
			}
		}
	}
}

// An unrelated commit landing on main between fetch and push is not a lost
// claim: the claimer revalidates and rebuilds on the new tip.
func TestGMClaimRebuildsOverUnrelatedCommit(t *testing.T) {
	remote, clones := gmRepo(t, 2)
	c := newClaimer(clones[0], "codex-b")
	fired := false
	c.beforePush = func() {
		if fired {
			return
		}
		fired = true
		other := clones[1]
		require.NoError(t, os.WriteFile(filepath.Join(other, "proj", "PLAN.md"), []byte("plan\n"), 0o644))
		gitT(t, other, "add", "-A")
		gitT(t, other, "commit", "-qm", "unrelated")
		gitT(t, other, "push", "-q", "origin", "HEAD:main")
	}
	r, err := c.Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimWon, r.Outcome)
	assert.Equal(t, "plan", gitT(t, remote, "show", "main:proj/PLAN.md"), "the unrelated commit is kept")
}

func TestGMClaimRefusesHeldOrUnlistedClaimant(t *testing.T) {
	remote, clones := gmRepo(t, 2)
	seed := clones[1]
	require.NoError(t, os.MkdirAll(filepath.Join(seed, "proj", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seed, "proj", "holds", "codex-b"), []byte("paused"), 0o644))
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-qm", "hold")
	gitT(t, seed, "push", "-q", "origin", "HEAD:main")

	r, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimRefused, r.Outcome, "a user hold is checked at the commit being built on")
	r, err = newClaimer(clones[0], "mallory").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimRefused, r.Outcome)
	r, err = newClaimer(clones[0], "claude").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimRefused, r.Outcome, "the GM cannot claim from itself")
	_ = remote
}

// A push whose result is unknown (a timeout after the remote accepted it) is
// reconciled by looking for the claim ID, never by minting another term.
func TestGMClaimReconcilesUncertainPush(t *testing.T) {
	remote, clones := gmRepo(t, 1)
	c := newClaimer(clones[0], "codex-b")
	c.pushErr = func(err error) error {
		if err == nil {
			return errUncertain
		}
		return err
	}
	r, err := c.Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimWon, r.Outcome, "the remote shows our claim ID")
	assert.Equal(t, 1, remoteGM(t, remote).Term, "no second term")
}

func TestGMAuthorityUnavailable(t *testing.T) {
	_, clones := gmRepo(t, 1)
	gitT(t, clones[0], "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	r, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimUnavailable, r.Outcome)
}

// Before every GM-file write the GM confirms the remote still names it with
// its claim ID; a GM whose term ended must stop.
func TestGMVerifyFencesOldGM(t *testing.T) {
	_, clones := gmRepo(t, 2)
	won, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	ok, _, err := newClaimer(clones[0], "codex-b").Verify(context.Background(), won.State.Term, won.State.ClaimID)
	require.NoError(t, err)
	assert.True(t, ok)

	_, err = newClaimer(clones[1], "codex-b").Release(context.Background(), won.State.Term, won.State.ClaimID, "claude")
	require.NoError(t, err)
	ok, cur, err := newClaimer(clones[0], "codex-b").Verify(context.Background(), won.State.Term, won.State.ClaimID)
	require.NoError(t, err)
	assert.False(t, ok, "the hand-back ended codex-b's term")
	assert.Equal(t, "claude", cur.GM)
	assert.Equal(t, 2, cur.Term)
	assert.Contains(t, cur.HandoffOf, won.State.ClaimID)
}

func TestGMReleaseRequiresCurrentAuthority(t *testing.T) {
	_, clones := gmRepo(t, 1)
	_, err := newClaimer(clones[0], "codex-b").Release(context.Background(), 0, "", "claude")
	assert.Error(t, err, "only the current GM, with its term and claim ID, can hand back")
}

// The listener reads authority from project-state's origin/main, so a claim
// pushed from another machine retires stale watches before anyone pulls.
func TestCohortListenerSeesRemoteClaimBeforePull(t *testing.T) {
	_, clones := gmRepo(t, 2)
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListenerAs(t, api, d, Identity{UserID: "UCB", BotID: "BCB"})
	now := time.Unix(1_800_000_000, 0)
	l.Now = func() time.Time { return now }
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	resp := l.Control(context.Background(), ControlRequest{Op: "cohort-register", SessionID: "s2",
		Cohort: &CohortReg{Project: "proj", Agent: "codex-b", Root: clones[0], Channel: "C1"}})
	require.True(t, resp.OK, resp.Error)

	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "1800000000.000100", User: "UBR", Text: "<@UCL> please decide"})
	_, err := newClaimer(clones[1], "codex-r").Claim(context.Background(), 0, "claude", "elsewhere")
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(clones[0], "proj", "gm.json"), "clone 0 has not pulled")

	now = now.Add(30 * time.Minute)
	l.CohortTick(context.Background())
	for _, g := range d.got {
		assert.NotContains(t, g.text, "[cohort]", "the watch on the old GM was retired by the remote claim")
	}
}
