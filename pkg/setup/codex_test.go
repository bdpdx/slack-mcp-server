package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notifyLine = `notify = ["/Apps/Client", "turn-ended", "--previous-notify", "[\"python3\",\"\\/u\\/notify\\/codex-push.py\"]"]`

// testBin is a writable stand-in for the linked binary; setup writes the
// Codex start script beside it.
var testBin = filepath.Join(os.TempDir(), "setup-test-bin", "slack-mcp-server")

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
	res, err := InstallCodex(home, t.TempDir(), testBin, r, p, testNow)
	require.NoError(t, err)

	hooks, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	assert.NotContains(t, string(hooks), "approval-hook", "auto_review homes get no approval hook")
	assert.Contains(t, string(hooks), "relay-hook")
	assert.Contains(t, string(hooks), `"description": "mine"`)

	rules, _ := os.ReadFile(filepath.Join(home, "rules", "default.rules"))
	assert.Equal(t, `prefix_rule(pattern=["`+testBin+`", "chat"], decision="allow")`+"\n", string(rules))

	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	assert.NotContains(t, string(cfg), "codex-push.py")
	assert.FileExists(t, filepath.Join(home, "config.toml.bak-20261005120000"))

	require.Len(t, r.calls, 4, "mcp remove/add, then launchctl enable/bootstrap")
	assert.True(t, strings.HasPrefix(r.calls[1], "CODEX_HOME="+home+" codex mcp add slack -- "+testBin))
	assert.NotEmpty(t, res.Changed)

	// Second run: nothing new, rule not duplicated, notify not asked again.
	_, err = InstallCodex(home, t.TempDir(), testBin, &fakeRunner{}, &Scripted{}, testNow)
	require.NoError(t, err)
	rules, _ = os.ReadFile(filepath.Join(home, "rules", "default.rules"))
	assert.Equal(t, 1, strings.Count(string(rules), "prefix_rule"))
}

func TestInstallCodexKeepsNotifyWhenDeclined(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(notifyLine+"\n"), 0o600))
	_, err := InstallCodex(home, t.TempDir(), testBin, &fakeRunner{}, &Scripted{Answers: []string{"n"}}, testNow)
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
	_, err := InstallCodex(home, t.TempDir(), testBin, &fakeRunner{}, &Scripted{}, testNow)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.NoFileExists(t, filepath.Join(home, "skills"))
	assert.NoFileExists(t, filepath.Join(home, "rules", "default.rules"))
}

func TestInstallCodexBacksUpConfigBeforeCLI(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("model = \"x\"\n"), 0o600))
	_, err := InstallCodex(home, t.TempDir(), testBin, &fakeRunner{}, &Scripted{}, testNow)
	require.NoError(t, err)
	got, err := os.ReadFile(cfg + ".bak-20261005120000")
	require.NoError(t, err, "config.toml is backed up before codex mcp edits it")
	assert.Equal(t, "model = \"x\"\n", string(got))

	// Without the codex CLI nothing edits config.toml: no backup.
	home2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home2, "config.toml"), []byte("model = \"x\"\n"), 0o600))
	_, err = InstallCodex(home2, t.TempDir(), testBin, &fakeRunner{missing: map[string]bool{"codex": true}}, &Scripted{}, testNow)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(home2, "config.toml.bak-20261005120000"))
}

func TestInstallCodexReplacesStaleRules(t *testing.T) {
	home := t.TempDir()
	rulesPath := filepath.Join(home, "rules", "default.rules")
	require.NoError(t, os.MkdirAll(filepath.Dir(rulesPath), 0o700))
	old := `prefix_rule(pattern=["/old/y", "chat"], decision="allow")` + "\n" +
		`prefix_rule(pattern=["git", "status"], decision="allow")` + "\n" +
		`prefix_rule(pattern=["/other/slack-mcp-server", "chat"], decision="allow")` + "\n" +
		`prefix_rule(pattern=["/x/slack-mcp-server", "setup"], decision="allow")` + "\n" +
		"# keep me\n"
	require.NoError(t, os.WriteFile(rulesPath, []byte(old), 0o600))

	_, err := InstallCodex(home, t.TempDir(), testBin, &fakeRunner{}, &Scripted{}, testNow)
	require.NoError(t, err)
	got, _ := os.ReadFile(rulesPath)
	assert.Equal(t, `prefix_rule(pattern=["/old/y", "chat"], decision="allow")`+"\n"+
		`prefix_rule(pattern=["git", "status"], decision="allow")`+"\n"+
		`prefix_rule(pattern=["/x/slack-mcp-server", "setup"], decision="allow")`+"\n"+
		"# keep me\n"+
		`prefix_rule(pattern=["`+testBin+`", "chat"], decision="allow")`+"\n", string(got))

	// Same path again: file unchanged, no new backup.
	before, _ := filepath.Glob(filepath.Join(home, "rules", "*"))
	res, err := InstallCodex(home, t.TempDir(), testBin, &fakeRunner{}, &Scripted{}, testNow.Add(time.Hour))
	require.NoError(t, err)
	again, _ := os.ReadFile(rulesPath)
	assert.Equal(t, string(got), string(again))
	after, _ := filepath.Glob(filepath.Join(home, "rules", "*"))
	assert.Equal(t, before, after)
	assert.NotContains(t, res.Changed, rulesPath)
}

func TestInstallCodexReplacesRuleForSameBinaryViaOtherLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o700))
	linkA, linkB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	require.NoError(t, os.Symlink(target, linkA))
	require.NoError(t, os.Symlink(target, linkB))
	home := t.TempDir()
	_, err := InstallCodex(home, t.TempDir(), linkA, &fakeRunner{}, &Scripted{}, testNow)
	require.NoError(t, err)
	_, err = InstallCodex(home, t.TempDir(), linkB, &fakeRunner{}, &Scripted{}, testNow)
	require.NoError(t, err)
	rules, _ := os.ReadFile(filepath.Join(home, "rules", "default.rules"))
	assert.Equal(t, `prefix_rule(pattern=["`+linkB+`", "chat"], decision="allow")`+"\n", string(rules))
}
