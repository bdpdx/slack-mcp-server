package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	assert.True(t, res[0].Installed)
	assert.Equal(t, TypeCodex, res[0].Type)
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
	assert.Contains(t, res[0].Notes[0], "Fix this, then run ./install.sh again and set up this home again.")
	assert.False(t, res[0].Installed)
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

func TestRunReinstallKeepsCustomToolValues(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	codex := filepath.Join(user, ".codex")
	require.NoError(t, os.MkdirAll(codex, 0o700))
	require.NoError(t, os.WriteFile(EnvPath(codex), []byte(
		"SLACK_MCP_XOXB_TOKEN=xoxb-old\nSLACK_MCP_ADD_MESSAGE_TOOL=C123,#general\nSLACK_MCP_DELETE_MESSAGE_TOOL='!C123 #x'\n"), 0o600))
	answers := []string{"1", "2", "y", "pat-codex", "y", "xapp-1", "xoxb-good", "xoxp-good", "n"}
	for range len(DefaultOnTools) + len(DefaultOffTools) - 2 {
		answers = append(answers, "") // keep each toggle's default
	}
	p := &Scripted{Answers: answers}
	res, err := Run(context.Background(), opts(user, repo, p))
	require.NoError(t, err)
	require.Len(t, res, 1)
	out := p.Out.String()
	assert.Contains(t, out, "SLACK_MCP_ADD_MESSAGE_TOOL=C123,#general: custom (kept)")
	assert.Contains(t, out, "SLACK_MCP_DELETE_MESSAGE_TOOL=!C123 #x: custom (kept)")
	assert.NotContains(t, out, "Enable SLACK_MCP_ADD_MESSAGE_TOOL?")
	assert.NotContains(t, out, "Enable SLACK_MCP_DELETE_MESSAGE_TOOL?")
	assert.Contains(t, out, "Enable SLACK_MCP_JOIN_TOOL?")

	env, err := ReadEnv(EnvPath(codex))
	require.NoError(t, err)
	assert.Equal(t, "C123,#general", env["SLACK_MCP_ADD_MESSAGE_TOOL"])
	assert.Equal(t, "!C123 #x", env["SLACK_MCP_DELETE_MESSAGE_TOOL"])
	assert.Equal(t, "xoxb-good", env["SLACK_MCP_XOXB_TOKEN"])
}

func TestRunFailedUpdateSaysChooseUpdate(t *testing.T) {
	user, repo := t.TempDir(), t.TempDir()
	claude := filepath.Join(user, ".claude")
	require.NoError(t, os.MkdirAll(claude, 0o700))
	require.NoError(t, os.WriteFile(EnvPath(claude), []byte("SLACK_MCP_XOXB_TOKEN=x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{"), 0o600))
	res, err := Run(context.Background(), opts(user, repo, &Scripted{Answers: []string{"1", "1"}}))
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Contains(t, strings.Join(res[0].Notes, "\n"), "Fix this, then run ./install.sh again and choose Update for this home.")
}

func TestPrintSummary(t *testing.T) {
	icon := fmt.Sprintf(iconMsgFmt, "pat-codex", "pat-codex")
	both := []Result{
		{Home: "/u/.claude", Type: TypeClaude, Installed: true},
		{Home: "/u/.codex", Type: TypeCodex, Installed: true, Notes: []string{icon}},
	}
	var b bytes.Buffer
	printSummary(&b, "/b/slack-mcp-server", both, false)
	out := b.String()
	assert.Contains(t, out, "slack-mcp-server is installed at /b/slack-mcp-server\n", "an unresolvable link is named alone")
	assert.Contains(t, out, "Remaining steps:")
	assert.Contains(t, out, "1. Set the bot icon")
	assert.Contains(t, out, "Trust the hooks when Codex asks.")

	b.Reset()
	printSummary(&b, "/b/slack-mcp-server", both[:1], false)
	out = b.String()
	assert.Contains(t, out, "Remaining steps:")
	assert.NotContains(t, out, "Codex", "no Codex home was set up")
	assert.NotContains(t, out, "bot icon", "no icon note was given")
	assert.Contains(t, out, "1. Restart your agent sessions.")

	b.Reset()
	printSummary(&b, "/b/slack-mcp-server", both, true)
	assert.NotContains(t, b.String(), "Remaining steps:", "not after an abort")
	assert.Contains(t, b.String(), "Setup stopped before it finished")

	b.Reset()
	printSummary(&b, "/b/slack-mcp-server", []Result{{Home: "/u/.codex", Type: TypeCodex, Notes: []string{"Skipped."}}}, false)
	assert.NotContains(t, b.String(), "Remaining steps:", "no home was set up")
}

// The summary says where the binary was linked and what the link points to.
func TestPrintSummaryNamesTheInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "build", "slack-mcp-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
	require.NoError(t, os.WriteFile(target, []byte("bin"), 0o700))
	link := filepath.Join(dir, "bin", "slack-mcp-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
	require.NoError(t, os.Symlink(target, link))
	var b bytes.Buffer
	printSummary(&b, link, []Result{{Home: "/u/.claude", Type: TypeClaude, Installed: true}}, false)
	real, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	assert.Contains(t, b.String(), "slack-mcp-server is installed at "+link+" (a link to "+real+")")
}

func TestBinPathIsAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	got, err := binPath("bin/slack-mcp-server")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd, "bin", "slack-mcp-server"), got)
	got, err = binPath("/opt/slack-mcp-server")
	require.NoError(t, err)
	assert.Equal(t, "/opt/slack-mcp-server", got)
	got, err = binPath("")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got), "defaults to this executable")
}
