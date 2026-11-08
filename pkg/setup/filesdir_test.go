package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setFilesDir writes the folder into the env file, keeps every other
// setting, and reports a change only when the folder in use differs.
func TestSetFilesDir(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	home := filepath.Join(user, ".claude-b")
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.WriteFile(EnvPath(home), []byte("SLACK_MCP_XOXB_TOKEN=xoxb-1\n"), 0o600))
	o := opts(user, t.TempDir(), &Scripted{})
	h := Home{Path: home, Type: TypeClaude, HasEnv: true}

	changed, err := setFilesDir(o, h)
	require.NoError(t, err)
	assert.False(t, changed, "no FilesDir leaves the home alone")
	env, _ := ReadEnv(EnvPath(home))
	assert.NotContains(t, env, "SLACK_MCP_FILES_DIR")

	o.FilesDir = filepath.Join(user, "Downloads", "slack-mcp")
	changed, err = setFilesDir(o, h)
	require.NoError(t, err)
	assert.False(t, changed, "writing the default the server already used is no change")
	env, _ = ReadEnv(EnvPath(home))
	assert.Equal(t, o.FilesDir, env["SLACK_MCP_FILES_DIR"])
	assert.Equal(t, "xoxb-1", env["SLACK_MCP_XOXB_TOKEN"])

	o.FilesDir = "/srv/slack files"
	changed, err = setFilesDir(o, h)
	require.NoError(t, err)
	assert.True(t, changed)
	env, _ = ReadEnv(EnvPath(home))
	assert.Equal(t, "/srv/slack files", env["SLACK_MCP_FILES_DIR"])
	info, _ := os.Stat(EnvPath(home))
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	changed, err = setFilesDir(o, h)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestFilesDirFlag(t *testing.T) {
	user := t.TempDir()
	got, err := filesDirFlag("", user)
	require.NoError(t, err)
	assert.Equal(t, "", got)

	got, err = filesDirFlag("~/slack-files", user)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(user, "slack-files"), got)

	for _, bad := range []string{"~", user, filepath.Dir(user), "/"} {
		_, err := filesDirFlag(bad, user)
		assert.Error(t, err, bad)
	}
	file := filepath.Join(user, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = filesDirFlag(file, user)
	assert.Error(t, err)
}

// An Update writes the files folder into the home's env file, saves it in
// the state for install.sh to offer next time, and marks the home's running
// sessions stale.
func TestRunUpdateSetsFilesDir(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", user)
	home := savedHome(t, repo, filepath.Join(user, ".claude-b"), TypeClaude)
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(EnvPath(home), []byte("SLACK_MCP_XOXB_TOKEN=xoxb-1\n"), 0o600))

	o := opts(user, repo, &Scripted{Answers: []string{"1", "1", "n"}}) // these homes; Update; no REPO_ROOT
	o.FilesDir = "/srv/slack-files"
	res, err := Run(context.Background(), o)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.True(t, res[0].Installed)
	assert.True(t, res[0].Stale)
	assert.Contains(t, res[0].Changed, EnvPath(home)+" (SLACK_MCP_FILES_DIR)")

	env, _ := ReadEnv(EnvPath(home))
	assert.Equal(t, "/srv/slack-files", env["SLACK_MCP_FILES_DIR"])
	assert.Equal(t, "xoxb-1", env["SLACK_MCP_XOXB_TOKEN"])
	st, _ := LoadState(filepath.Join(repo, ".install-state.json"))
	assert.Equal(t, "/srv/slack-files", st.FilesDir)
}
