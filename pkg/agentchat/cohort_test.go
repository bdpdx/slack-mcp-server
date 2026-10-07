package agentchat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeProject(t *testing.T, root, project, projectMD string) {
	t.Helper()
	dir := filepath.Join(root, project)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PROJECT.md"), []byte(projectMD), 0o644))
}

func TestParseSuccession(t *testing.T) {
	// The machine-readable marker wins.
	got, err := ParseSuccession("# p\n<!-- cohort-succession: claude, codex-b, codex-r, claude-r -->\n**GM succession order:** x (GM), then y.\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "codex-b", "codex-r", "claude-r"}, got)

	// Without it, the prose line from the project template is parsed.
	got, err = ParseSuccession("**GM succession order:** claude (GM), then codex-b, codex-r, claude-r.\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "codex-b", "codex-r", "claude-r"}, got)

	_, err = ParseSuccession("# a project with no succession order\n")
	assert.Error(t, err)
	_, err = ParseSuccession("<!-- cohort-succession: claude, claude -->")
	assert.Error(t, err, "duplicates are refused")
}

func TestLoadCohortProject(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, "2026-10-06-ui-fixes", "<!-- cohort-succession: claude, codex-b -->\n")

	p, err := LoadCohortProject(root, "2026-10-06-ui-fixes")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "codex-b"}, p.Succession)
	assert.Equal(t, GMState{Term: 0, GM: "claude"}, p.GM, "no gm.json: the first in the order is GM at term 0")
	assert.True(t, p.InRoster("codex-b"))
	assert.False(t, p.InRoster("mallory"))

	require.NoError(t, os.WriteFile(filepath.Join(root, "2026-10-06-ui-fixes", "gm.json"),
		[]byte(`{"term":3,"gm":"codex-b","claim_id":"c1","since":"2026-10-07T12:00:00-07:00"}`), 0o644))
	p, err = LoadCohortProject(root, "2026-10-06-ui-fixes")
	require.NoError(t, err)
	assert.Equal(t, "codex-b", p.GM.GM)
	assert.Equal(t, 3, p.GM.Term)
	assert.Equal(t, "c1", p.GM.ClaimID)

	require.NoError(t, os.MkdirAll(filepath.Join(root, "2026-10-06-ui-fixes", "holds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "2026-10-06-ui-fixes", "holds", "claude"), []byte("paused by Brian"), 0o644))
	p, err = LoadCohortProject(root, "2026-10-06-ui-fixes")
	require.NoError(t, err)
	assert.True(t, p.Held("claude"))
	assert.False(t, p.Held("codex-b"))

	_, err = LoadCohortProject(root, "missing")
	assert.Error(t, err)
	_, err = LoadCohortProject(root, "../escape")
	assert.Error(t, err, "project names are plain directory names")
}

func TestSuccessors(t *testing.T) {
	p := &CohortProject{Succession: []string{"claude", "codex-b", "codex-r", "claude-r"}, GM: GMState{GM: "claude"},
		holds: map[string]bool{"codex-r": true}}
	assert.Equal(t, []string{"codex-b", "claude-r"}, p.Successors(), "the GM and held agents are skipped, order kept")
	p.GM.GM = "codex-b"
	assert.Equal(t, []string{"claude", "claude-r"}, p.Successors())
}

// Every listener must compute the same escalation step from the same facts,
// so the step depends only on elapsed time.
func TestEscalationStep(t *testing.T) {
	seen := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		after time.Duration
		step  int
	}{
		{0, -1}, {14 * time.Minute, -1}, {15 * time.Minute, 0}, {24 * time.Minute, 0},
		{25 * time.Minute, 1}, {35 * time.Minute, 2},
	} {
		assert.Equal(t, tc.step, EscalationStep(seen, seen.Add(tc.after)), tc.after.String())
	}
}

func TestCheckpointDue(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r := &CohortReg{RegisteredAt: start}
	assert.False(t, r.CheckpointDue(start.Add(119*time.Minute)))
	assert.True(t, r.CheckpointDue(start.Add(2*time.Hour)))

	r.CheckpointNotified = start.Add(2 * time.Hour)
	assert.False(t, r.CheckpointDue(start.Add(3*time.Hour)), "one notice per due point")
	assert.True(t, r.CheckpointDue(start.Add(4*time.Hour)), "still not done: reminded at the next due point")

	r.CheckpointNotified = start.Add(time.Hour + 59*time.Minute)
	assert.True(t, r.CheckpointDue(start.Add(9*time.Hour)), "back after a long gap: due")
	r.CheckpointNotified = start.Add(9 * time.Hour)
	assert.False(t, r.CheckpointDue(start.Add(9*time.Hour+time.Minute)), "and only once: missed ones coalesce")

	r.LastCheckpoint = start.Add(9 * time.Hour)
	assert.False(t, r.CheckpointDue(start.Add(10*time.Hour)), "anchored to the last completed checkpoint")
	assert.True(t, r.CheckpointDue(start.Add(11*time.Hour)))
}

func TestProjectStateRoot(t *testing.T) {
	state := t.TempDir()
	clone := t.TempDir()
	require.NoError(t, os.Symlink(state, filepath.Join(clone, "projects")))
	deep := filepath.Join(clone, "a", "b")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	want, err := filepath.EvalSymlinks(state)
	require.NoError(t, err)

	got, err := projectStateRoot(deep)
	require.NoError(t, err)
	assert.Equal(t, want, got, "found from a subdirectory, resolved to the real path")

	_, err = projectStateRoot(t.TempDir())
	assert.Error(t, err)

	plain := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(plain, "projects"), 0o755))
	_, err = projectStateRoot(plain)
	assert.Error(t, err, "a plain projects/ directory is not the project-state link")
}
