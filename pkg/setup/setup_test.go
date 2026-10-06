package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeValidator struct {
	taken  bool
	checkE error
}

func (f fakeValidator) AuthTest(_ context.Context, token string) (Identity, error) {
	switch token {
	case "xoxb-good":
		return Identity{TeamID: "T1", UserID: "UB", User: "newbot", IsBot: true}, nil
	case "xoxb-bad":
		return Identity{}, errors.New("invalid_auth")
	case "xoxp-dotted":
		return Identity{TeamID: "T1", UserID: "UP", User: "pat.d"}, nil
	}
	return Identity{TeamID: "T1", UserID: "UP", User: "pat"}, nil
}
func (fakeValidator) CheckAppToken(context.Context, string) error { return nil }
func (f fakeValidator) BotNameTaken(context.Context, string, string, string) (bool, error) {
	return f.taken, f.checkE
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
		"xapp-1", "xoxp-wrong", // bot slot given a user token: prefix failure, re-ask bot

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

func TestRunLiveCheckReasksOnlyBotToken(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	codex := filepath.Join(user, ".codex")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	p := &Scripted{Answers: []string{
		"1", "pat-codex", "y",
		"xapp-1", "xoxb-bad", "xoxp-good", // Slack rejects the bot token
		"xoxb-good", // only the bot token is asked again
		"y",
	}}
	_, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	assert.Contains(t, p.Out.String(), "invalid_auth")
	env, _ := ReadEnv(EnvPath(codex))
	assert.Equal(t, "xoxb-good", env["SLACK_MCP_XOXB_TOKEN"])
	assert.Equal(t, "xoxp-good", env["SLACK_MCP_XOXP_TOKEN"])
}

func runWithValidator(t *testing.T, v fakeValidator) string {
	user, repo := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(user, ".codex"), 0o700))
	p := &Scripted{Answers: []string{"1", "pat-codex", "y", "xapp-1", "xoxb-good", "xoxp-good", "y"}}
	o := opts(user, repo, p)
	o.V = v
	_, err := Run(context.Background(), o)
	require.NoError(t, err)
	return p.Out.String()
}

func TestRunBotNameCheckOutcomes(t *testing.T) {
	assert.Contains(t, runWithValidator(t, fakeValidator{taken: true}), `another bot in this workspace is already named "pat-codex"`)
	assert.Contains(t, runWithValidator(t, fakeValidator{checkE: errors.New("boom")}), `Could not check whether "pat-codex" is already used by another bot: boom`)
}

func TestAddHomeRejectsEmptyAndDuplicate(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	codex := filepath.Join(user, ".codex")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	extra := filepath.Join(user, "extra")
	require.NoError(t, os.MkdirAll(extra, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(extra, "settings.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(EnvPath(extra), []byte("SLACK_MCP_XOXB_TOKEN=x\n"), 0o600))
	require.NoError(t, os.WriteFile(EnvPath(codex), []byte("SLACK_MCP_XOXB_TOKEN=x\n"), 0o600))
	p := &Scripted{Answers: []string{"2", "", codex, extra, "1", "3", "3"}} // add: empty, duplicate, ok; set up; skip both
	res, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	assert.Len(t, res, 2)
	assert.Contains(t, p.Out.String(), "A path is required.")
	assert.Contains(t, p.Out.String(), "already in the list")
}

func TestRunSavesStateWithoutCompletedHome(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(user, ".codex"), 0o700))
	_, err := Run(context.Background(), opts(user, repo, &Scripted{Answers: []string{"1", "n"}}))
	require.ErrorIs(t, err, ErrAborted)
	_, err = os.Stat(filepath.Join(repo, ".install-state.json"))
	assert.NoError(t, err)
}

func TestExistingBinFor(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	assert.Equal(t, "", existingBinFor(user, repo))

	claude := filepath.Join(user, ".claude")
	require.NoError(t, os.MkdirAll(claude, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/opt/bin/slack-mcp-server chat --env-file /x relay-hook"}]}]}}`), 0o600))
	assert.Equal(t, "/opt/bin/slack-mcp-server", existingBinFor(user, repo))
}
