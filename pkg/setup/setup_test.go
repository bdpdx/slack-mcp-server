package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeValidator struct{ taken bool }

func (f fakeValidator) AuthTest(_ context.Context, token string) (Identity, error) {
	switch token {
	case "xoxb-good":
		return Identity{TeamID: "T1", UserID: "UB", User: "newbot", IsBot: true}, nil
	case "xoxp-dotted":
		return Identity{TeamID: "T1", UserID: "UP", User: "pat.d"}, nil
	}
	return Identity{TeamID: "T1", UserID: "UP", User: "pat"}, nil
}
func (fakeValidator) CheckAppToken(context.Context, string) error { return nil }
func (f fakeValidator) BotNameTaken(context.Context, string, string, string) (bool, error) {
	return f.taken, nil
}

func opts(user, repo string, p Prompter) Options {
	return Options{Repo: repo, Bin: "/b/slack-mcp-server", UserHome: user,
		P: p, V: fakeValidator{}, R: &fakeRunner{}, Now: func() time.Time { return testNow }}
}

func TestRunNewCodexHome(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	codex := filepath.Join(user, ".codex")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	p := &Scripted{Answers: []string{
		"1",         // set up these homes
		"my_bot",    // invalid: warned, asked again
		"pat-codex", // bot name
		"n",         // app does not exist yet
		"",          // Enter after installing the app
		" xapp-1 ",  // app token
		"xoxb-good", // bot token
		"xoxp-good", // user token
		"y",         // default tools
	}}
	res, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Contains(t, p.Out.String(), "separators in channel names")
	assert.Contains(t, p.Out.String(), "unique in this Slack workspace")
	assert.Contains(t, p.Out.String(), "Display Information")
	assert.Contains(t, p.Out.String(), "OAuth & Permissions")

	env, err := ReadEnv(EnvPath(codex))
	require.NoError(t, err)
	assert.Equal(t, "xapp-1", env["SLACK_MCP_XAPP_TOKEN"])
	info, _ := os.Stat(EnvPath(codex))
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.FileExists(t, filepath.Join(repo, ".install", "manifests", "pat-codex.json"))

	st, _ := LoadState(filepath.Join(repo, ".install-state.json"))
	assert.Equal(t, "pat-codex", st.Home(codex).Bot)
}

func TestRunUpdateKeepsEnv(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	claude := filepath.Join(user, ".claude")
	require.NoError(t, os.MkdirAll(claude, 0o700))
	original := "SLACK_MCP_XOXB_TOKEN=xoxb-keep\n"
	require.NoError(t, os.WriteFile(EnvPath(claude), []byte(original), 0o600))
	p := &Scripted{Answers: []string{"1", "1"}} // set up; Update
	_, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	data, _ := os.ReadFile(EnvPath(claude))
	assert.Equal(t, original, string(data), "Update never touches the env file")
	assert.FileExists(t, filepath.Join(claude, "skills", "slack-agent-chat", "SKILL.md"))
}

func TestRunAbortsOnEOF(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(user, ".codex"), 0o700))
	_, err := Run(context.Background(), opts(user, repo, &Scripted{Answers: []string{"1", "pat-codex"}}))
	assert.ErrorIs(t, err, ErrAborted)
	assert.NoFileExists(t, EnvPath(filepath.Join(user, ".codex")))
}

func TestRunReasksOnlyBadToken(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	codex := filepath.Join(user, ".codex")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	p := &Scripted{Answers: []string{
		"1", "pat-codex", "y", // set up; name; app exists
		"xapp-1", "xoxp-wrong", "xoxp-good", // bot slot given a user token: prefix problem, re-ask bot
		"xoxb-good", "xoxp-good", "y",
	}}
	_, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	assert.Contains(t, p.Out.String(), "should start with xoxb-")
	env, _ := ReadEnv(EnvPath(codex))
	assert.Equal(t, "xoxb-good", env["SLACK_MCP_XOXB_TOKEN"])
}

func TestRunDottedUsernameFailsHomeOnly(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	codex := filepath.Join(user, ".codex")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	p := &Scripted{Answers: []string{"1", "pat-codex", "y", "xapp-1", "xoxb-good", "xoxp-dotted"}}
	res, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.NotEmpty(t, res[0].Notes)
	assert.Contains(t, res[0].Notes[0], "FAILED")
	assert.Contains(t, res[0].Notes[0], "pat.d")
	assert.NoFileExists(t, EnvPath(codex))
}
