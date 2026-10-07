package agentchat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
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

	gitT(t, clones[0], "update-ref", "-d", "refs/remotes/origin/main")
	assert.ErrorContains(t, checkProjectState(context.Background(), clones[0]), "with origin/main")
}
