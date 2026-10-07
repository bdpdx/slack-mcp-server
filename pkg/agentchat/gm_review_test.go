package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A hand-back racing another authority update must be lost, never
// rebuilt over the other claim.
func TestGMHandBackRaceIsLostNotRebuilt(t *testing.T) {
	remote, clones := gmRepo(t, 2)
	won, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	require.Equal(t, ClaimWon, won.Outcome)
	rel := newClaimer(clones[0], "codex-b")
	fired := false
	rel.beforePush = func() {
		if fired {
			return
		}
		fired = true
		r, err := newClaimer(clones[1], "codex-r").Claim(context.Background(), 1, "codex-b", "codex-b silent")
		require.NoError(t, err)
		require.Equal(t, ClaimWon, r.Outcome)
	}
	r, err := rel.Release(context.Background(), won.State.Term, won.State.ClaimID, "claude")
	assert.Error(t, err)
	assert.Equal(t, ClaimLost, r.Outcome)
	g := remoteGM(t, remote)
	assert.Equal(t, 2, g.Term)
	assert.Equal(t, "codex-r", g.GM)
}

// The first push is refused by the server, but its commit lands on main
// anyway before the rebuilt second attempt pushes (a late-landing push the
// client had given up on). The claimer is GM on the remote, so it must be
// told it won, not lost.
func TestGMLateLandingPushIsWon(t *testing.T) {
	remote, clones := gmRepo(t, 1)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	shaFile := filepath.Join(t.TempDir(), "sha")
	marker := filepath.Join(t.TempDir(), "once")
	script := "#!/bin/sh\nread old new ref\nif [ ! -e " + marker + " ]; then touch " + marker + "; echo $new > " + shaFile + "; exit 1; fi\nexit 0\n"
	require.NoError(t, os.WriteFile(hook, []byte(script), 0o755))
	c := newClaimer(clones[0], "codex-b")
	attempt := 0
	c.beforePush = func() {
		attempt++
		if attempt == 2 {
			sha, err := os.ReadFile(shaFile)
			require.NoError(t, err)
			gitT(t, clones[0], "push", "-q", "origin", string(sha[:40])+":refs/heads/main")
		}
	}
	r, err := c.Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	g := remoteGM(t, remote)
	t.Logf("outcome=%s reason=%q remote=%+v", r.Outcome, r.Reason, g)
	assert.Equal(t, "codex-b", g.GM)
	assert.Equal(t, 1, g.Term)
	assert.Equal(t, ClaimWon, r.Outcome, "a claim ID minted earlier in this call is recognized on the remote")
}

// A remote that always declines (a branch rule) is reported as declined,
// not as main moving.
func TestGMDeclinedPushReportsTheReason(t *testing.T) {
	remote, clones := gmRepo(t, 1)
	require.NoError(t, os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\necho protected >&2\nexit 1\n"), 0o755))
	r, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimUnavailable, r.Outcome)
	assert.Contains(t, r.Reason, "declined")
}

func TestGMReleaseToSelfIsRefused(t *testing.T) {
	_, clones := gmRepo(t, 1)
	won, err := newClaimer(clones[0], "codex-b").Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	r, err := newClaimer(clones[0], "codex-b").Release(context.Background(), won.State.Term, won.State.ClaimID, "codex-b")
	assert.Error(t, err)
	assert.Equal(t, ClaimRefused, r.Outcome)
}

func TestGMInitGivesTheFirstGMATerm(t *testing.T) {
	remote, clones := gmRepo(t, 1)
	r, err := newClaimer(clones[0], "codex-b").Init(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ClaimRefused, r.Outcome, "only the first in the succession order")
	r, err = newClaimer(clones[0], "claude").Init(context.Background())
	require.NoError(t, err)
	require.Equal(t, ClaimWon, r.Outcome)
	assert.Equal(t, GMState{Term: 1, GM: "claude", ClaimID: r.State.ClaimID, Since: r.State.Since, Reason: "first GM"}, remoteGM(t, remote))
	ok, _, err := newClaimer(clones[0], "claude").Verify(context.Background(), 1, r.State.ClaimID)
	require.NoError(t, err)
	assert.True(t, ok, "the first GM can now fence its GM-file writes")
	r, err = newClaimer(clones[0], "claude").Init(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ClaimLost, r.Outcome, "only once")
}

func TestGMLockWaitHonorsCancellation(t *testing.T) {
	_, clones := gmRepo(t, 1)
	held := newClaimer(clones[0], "codex-b")
	unlock, err := held.lock(context.Background())
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := newClaimer(clones[0], "codex-r").Claim(ctx, 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimUnavailable, r.Outcome)
	assert.Less(t, time.Since(start), 5*time.Second, "a held lock cannot stall a claim past its deadline")
}

// The receipt needs the exact proposed term, GM and claim ID.
func TestGMReceiptRequiresExactState(t *testing.T) {
	remote, clones := gmRepo(t, 2)
	c := newClaimer(clones[0], "codex-b")
	c.pushErr = func(err error) error {
		if err != nil {
			return err
		}
		// Someone rewrites gm.json on top, keeping our claim ID but changing the GM.
		other := clones[1]
		gitT(t, other, "pull", "-q", "origin", "main")
		var g GMState
		require.NoError(t, json.Unmarshal([]byte(gitT(t, remote, "show", "main:proj/gm.json")), &g))
		g.GM = "codex-r"
		data, _ := json.Marshal(g)
		require.NoError(t, os.WriteFile(filepath.Join(other, "proj", "gm.json"), data, 0o644))
		gitT(t, other, "commit", "-qam", "tamper")
		gitT(t, other, "push", "-q", "origin", "HEAD:main")
		return nil
	}
	r, err := c.Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.NotEqual(t, ClaimWon, r.Outcome)
}

// Admission is rechecked before every attempt, including a rebuild.
func TestGMAdmissionRecheckedOnRebuild(t *testing.T) {
	_, clones := gmRepo(t, 2)
	c := newClaimer(clones[0], "codex-b")
	calls := 0
	c.Admit = func(context.Context) error {
		calls++
		if calls > 1 {
			return errors.New("the deadline was answered")
		}
		return nil
	}
	fired := false
	c.beforePush = func() {
		if fired {
			return
		}
		fired = true
		other := clones[1]
		require.NoError(t, os.WriteFile(filepath.Join(other, "proj", "NOTES.md"), []byte("x\n"), 0o644))
		gitT(t, other, "add", "-A")
		gitT(t, other, "commit", "-qm", "unrelated")
		gitT(t, other, "push", "-q", "origin", "HEAD:main")
	}
	r, err := c.Claim(context.Background(), 0, "claude", "x")
	require.NoError(t, err)
	assert.Equal(t, ClaimRefused, r.Outcome, "the rebuild re-ran admission and it no longer holds")
	assert.Equal(t, 2, calls)
}
