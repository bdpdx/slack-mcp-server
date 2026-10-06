package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notifyLine = `notify = ["/Apps/Client", "turn-ended", "--previous-notify", "[\"python3\",\"\\/u\\/notify\\/codex-push.py\"]"]`

func TestUsesAutoReview(t *testing.T) {
	assert.True(t, usesAutoReview("model = \"x\"\napprovals_reviewer = \"auto_review\"\n"))
	assert.False(t, usesAutoReview("# approvals_reviewer = \"auto_review\"\n"))
	assert.False(t, usesAutoReview("[profile]\napprovals_reviewer = \"user\"\n"))
}

func TestFixNotify(t *testing.T) {
	out, changed, err := fixNotify("a = 1\n" + notifyLine + "\nb = 2\n")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Contains(t, out, `notify = ["/Apps/Client", "turn-ended"]`)
	assert.Contains(t, out, "a = 1\n")
	assert.Contains(t, out, "b = 2\n")

	same, changed, _ := fixNotify(`notify = ["/Apps/Client", "turn-ended"]` + "\n")
	assert.False(t, changed)
	assert.Equal(t, `notify = ["/Apps/Client", "turn-ended"]`+"\n", same)
}

func TestInstallCodex(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("approvals_reviewer = \"auto_review\"\n"+notifyLine+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "hooks.json"), []byte(`{"description":"mine","hooks":{"PermissionRequest":[{"hooks":[{"type":"command","command":"/old/slack-mcp-server chat --env-file /x approval-hook"}]}]}}`), 0o600))
	r := &fakeRunner{}
	p := &Scripted{Answers: []string{"y"}} // remove codex-push.py from notify
	res, err := InstallCodex(home, "/b/slack-mcp-server", r, p, testNow)
	require.NoError(t, err)

	hooks, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	assert.NotContains(t, string(hooks), "approval-hook", "auto_review homes get no approval hook")
	assert.Contains(t, string(hooks), "relay-hook")
	assert.Contains(t, string(hooks), `"description": "mine"`)

	rules, _ := os.ReadFile(filepath.Join(home, "rules", "default.rules"))
	assert.Equal(t, `prefix_rule(pattern=["/b/slack-mcp-server", "chat"], decision="allow")`+"\n", string(rules))

	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	assert.NotContains(t, string(cfg), "codex-push.py")
	assert.FileExists(t, filepath.Join(home, "config.toml.bak-20261005120000"))

	require.Len(t, r.calls, 2)
	assert.True(t, strings.HasPrefix(r.calls[1], "CODEX_HOME="+home+" codex mcp add slack -- /b/slack-mcp-server"))
	assert.NotEmpty(t, res.Changed)

	// Second run: nothing new, rule not duplicated, notify not asked again.
	_, err = InstallCodex(home, "/b/slack-mcp-server", &fakeRunner{}, &Scripted{}, testNow)
	require.NoError(t, err)
	rules, _ = os.ReadFile(filepath.Join(home, "rules", "default.rules"))
	assert.Equal(t, 1, strings.Count(string(rules), "prefix_rule"))
}

func TestInstallCodexKeepsNotifyWhenDeclined(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(notifyLine+"\n"), 0o600))
	_, err := InstallCodex(home, "/b/slack-mcp-server", &fakeRunner{}, &Scripted{Answers: []string{"n"}}, testNow)
	require.NoError(t, err)
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	assert.Contains(t, string(cfg), "codex-push.py")
	hooks, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	assert.Contains(t, string(hooks), "approval-hook", "no auto_review: the approval hook is installed")
}

func TestInstallCodexRejectsNonObjectHooks(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":[1]}`), 0o600))
	_, err := InstallCodex(home, "/b/slack-mcp-server", &fakeRunner{}, &Scripted{}, testNow)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.NoFileExists(t, filepath.Join(home, "skills"))
	assert.NoFileExists(t, filepath.Join(home, "rules", "default.rules"))
}
