package agentchat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectStateOriginPattern(t *testing.T) {
	for _, u := range []string{
		"git@github.com:rezilient-co/rezilient-project-state.git",
		"https://github.com/rezilient-co/rezilient-project-state",
		"https://github.com/rezilient-co/rezilient-project-state.git/",
		"ssh://git@github.com/rezilient-co/rezilient-project-state.git",
	} {
		assert.True(t, projectStateOrigin.MatchString(u), u)
	}
	for _, u := range []string{
		"git@github.com:rezilient-co/rezilient.git",
		"git@github.com:mallory/rezilient-co/rezilient-project-state-fork.git",
		"https://github.com/someone/rezilient-project-state.git",
		"/tmp/remote.git",
		"https://evil.example/rezilient-co/rezilient-project-state.git",
		"/tmp/rezilient-co/rezilient-project-state",
		"https://github.com.evil.example/rezilient-co/rezilient-project-state.git",
		"https://github.com@evil.example/rezilient-co/rezilient-project-state.git",
		"https://github.com/rezilient-co/rezilient-project-state.git?x=1",
		"https://github.com/rezilient-co/rezilient-project-state/../other",
	} {
		assert.False(t, projectStateOrigin.MatchString(u), u)
	}
}

func TestCheckProjectStateRefusesOtherOrigins(t *testing.T) {
	_, clones := gmRepo(t, 1)
	err := checkProjectState(context.Background(), clones[0])
	assert.ErrorContains(t, err, "is not rezilient-co/rezilient-project-state", "a local bare remote is not project-state")

	gitT(t, clones[0], "remote", "set-url", "origin", "git@github.com:rezilient-co/rezilient-project-state.git")
	assert.NoError(t, checkProjectState(context.Background(), clones[0]), "origin/main is already fetched")

	gitT(t, clones[0], "config", "remote.origin.pushurl", "git@github.com:someone/elsewhere.git")
	assert.ErrorContains(t, checkProjectState(context.Background(), clones[0]), "someone/elsewhere", "claims would be pushed elsewhere")
	gitT(t, clones[0], "config", "remote.origin.pushurl", "git@github.com:rezilient-co/rezilient-project-state.git")
	gitT(t, clones[0], "config", "--add", "remote.origin.pushurl", "git@github.com:someone/second.git")
	assert.ErrorContains(t, checkProjectState(context.Background(), clones[0]), "someone/second", "every push URL is checked")
	gitT(t, clones[0], "config", "--unset-all", "remote.origin.pushurl")
	gitT(t, clones[0], "config", "url.git@github.com:someone/.pushInsteadOf", "git@github.com:rezilient-co/")
	assert.ErrorContains(t, checkProjectState(context.Background(), clones[0]), "someone/", "pushInsteadOf is applied")
	gitT(t, clones[0], "config", "--unset", "url.git@github.com:someone/.pushInsteadOf")
	assert.NoError(t, checkProjectState(context.Background(), clones[0]))

	gitT(t, clones[0], "update-ref", "-d", "refs/remotes/origin/main")
	assert.ErrorContains(t, checkProjectState(context.Background(), clones[0]), "with origin/main")
}

func TestCheckpointContextIsLoggedPrivately(t *testing.T) {
	c := &cli{home: Home{StateDir: filepath.Join(t.TempDir(), "state")}}
	require.NoError(t, os.MkdirAll(c.home.StateDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(c.home.StateDir, "checkpoints.log"), nil, 0o644))
	require.NoError(t, c.logCheckpoint("proj", " on track ", "context fine"))
	require.NoError(t, c.logCheckpoint("proj", "still on track", "compacted once"))
	path := filepath.Join(c.home.StateDir, "checkpoints.log")
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)
	var first map[string]string
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	assert.Equal(t, "proj", first["project"])
	assert.Equal(t, "on track", first["drift"])
	assert.Equal(t, "context fine", first["context"])
}
