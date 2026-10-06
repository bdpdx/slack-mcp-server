package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type unEnv struct {
	user, repo, bin, real string
	r                     *fakeRunner
	p                     *Scripted
}

func newUnEnv(t *testing.T) *unEnv {
	e := &unEnv{user: t.TempDir(), repo: t.TempDir(), r: &fakeRunner{}, p: &Scripted{}}
	e.real = filepath.Join(e.repo, "build", "slack-mcp-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(e.real), 0o755))
	require.NoError(t, os.WriteFile(e.real, []byte("x"), 0o755))
	e.bin = filepath.Join(e.user, ".local", "bin", "slack-mcp-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(e.bin), 0o755))
	require.NoError(t, os.Symlink(e.real, e.bin))
	return e
}

func (e *unEnv) state(t *testing.T, homes ...HomeState) {
	require.NoError(t, (&State{Bin: e.bin, Homes: homes}).Save(filepath.Join(e.repo, ".install-state.json")))
}

func (e *unEnv) loadState(t *testing.T) *State {
	st, err := LoadState(filepath.Join(e.repo, ".install-state.json"))
	require.NoError(t, err)
	return st
}

func (e *unEnv) answer(a ...string) { e.p.Answers = a }

func (e *unEnv) run(t *testing.T) []Result {
	res, err := Uninstall(Options{Repo: e.repo, UserHome: e.user, Bin: e.bin, P: e.p, R: e.r,
		Now: func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }})
	require.NoError(t, err)
	return res
}

func (e *unEnv) hook(home, name string) string { return HookCommand(e.bin, EnvPath(home), name) }

func writeJSON(t *testing.T, path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func readJSON(t *testing.T, path string) map[string]any {
	doc, err := readJSONObject(path)
	require.NoError(t, err)
	return doc
}

func hookEntry(cmd string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}
}

// claudeHome builds a Claude home with our hooks, someone else's hook, the
// skill and an env file.
func (e *unEnv) claudeHome(t *testing.T, dir string) string {
	home := filepath.Join(e.user, dir)
	writeJSON(t, filepath.Join(home, "settings.json"), map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"UserPromptSubmit": []any{hookEntry("/usr/bin/other"), hookEntry(e.hook(home, "relay-hook"))},
			"Stop":             []any{hookEntry(e.hook(home, "stop-hook"))},
		},
	})
	_, err := InstallSkill(home, TypeClaude, e.bin, time.Now())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(EnvPath(home), []byte("SLACK_MCP_XOXB_TOKEN=xoxb-1\n"), 0o600))
	return home
}

func (e *unEnv) codexHome(t *testing.T, dir string) string {
	home := filepath.Join(e.user, dir)
	writeJSON(t, filepath.Join(home, "hooks.json"), map[string]any{
		"hooks": map[string]any{
			"Stop":              []any{hookEntry("/usr/bin/other-stop"), hookEntry(e.hook(home, "stop-hook"))},
			"PermissionRequest": []any{hookEntry(e.hook(home, "approval-hook") + " --wait 5m")},
		},
	})
	_, err := InstallSkill(home, TypeCodex, e.bin, time.Now())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(EnvPath(home), []byte("x=1\n"), 0o600))
	rules := filepath.Join(home, "rules", "default.rules")
	require.NoError(t, os.MkdirAll(filepath.Dir(rules), 0o700))
	require.NoError(t, os.WriteFile(rules, []byte("prefix_rule(pattern=[\"git\", \"status\"], decision=\"allow\")\n"+codexRule(e.bin)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\n"), 0o600))
	return home
}

func (e *unEnv) launchAgent(t *testing.T, home string) (plist, target string) {
	label := appServerLabel(home)
	plist = filepath.Join(e.user, "Library", "LaunchAgents", label+".plist")
	require.NoError(t, os.MkdirAll(filepath.Dir(plist), 0o755))
	require.NoError(t, os.WriteFile(plist, []byte("<plist/>"), 0o644))
	return plist, "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
}

func (e *unEnv) startScript(t *testing.T, home string) string {
	p := filepath.Join(filepath.Dir(e.bin), startScriptName(home))
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/bash\nexport PROJECT_ROOT='/p'\n"), 0o755))
	return p
}

func baks(t *testing.T, path string) []string {
	m, err := filepath.Glob(path + ".bak-*")
	require.NoError(t, err)
	return m
}

func TestUninstallDefaultIsNo(t *testing.T) {
	e := newUnEnv(t)
	home := e.claudeHome(t, ".claude")
	e.state(t, HomeState{Path: home, Type: TypeClaude})
	e.answer("") // Enter at the home question
	res := e.run(t)
	assert.True(t, exists(EnvPath(home)))
	assert.True(t, exists(filepath.Join(home, "skills", "slack-agent-chat", "SKILL.md")))
	assert.Empty(t, e.r.calls)
	assert.Len(t, e.loadState(t).Homes, 1)
	_, err := os.Lstat(e.bin)
	assert.NoError(t, err, "link stays while a home remains")
	require.Len(t, res, 1)
	assert.Contains(t, res[0].Notes[0], "Skipped")
}

func TestUninstallClaudeDefaultHome(t *testing.T) {
	e := newUnEnv(t)
	home := e.claudeHome(t, ".claude")
	e.state(t, HomeState{Path: home, Type: TypeClaude, Bot: "claude"})
	e.answer("y", "y") // uninstall home; remove the link
	e.run(t)

	doc := readJSON(t, filepath.Join(home, "settings.json"))
	assert.Equal(t, "dark", doc["theme"])
	data, _ := json.Marshal(doc["hooks"])
	assert.Contains(t, string(data), "/usr/bin/other")
	assert.NotContains(t, string(data), "relay-hook")
	assert.NotContains(t, string(data), "Stop", "emptied event dropped")
	assert.NotEmpty(t, baks(t, filepath.Join(home, "settings.json")))

	skillDir := filepath.Join(home, "skills", "slack-agent-chat")
	assert.False(t, exists(filepath.Join(skillDir, "SKILL.md")))
	assert.NotEmpty(t, baks(t, filepath.Join(skillDir, "SKILL.md")), "skill file backed up")
	assert.True(t, exists(skillDir), "folder kept: it holds the backups")
	assert.False(t, exists(EnvPath(home)))
	assert.Empty(t, baks(t, EnvPath(home)), "no backup of the tokens")
	assert.Equal(t, []string{"-u CLAUDE_CONFIG_DIR claude mcp remove -s user slack"}, e.r.calls)
	assert.Empty(t, e.loadState(t).Homes)
	_, err := os.Lstat(e.bin)
	assert.True(t, os.IsNotExist(err), "link removed")
	assert.True(t, exists(e.real), "target kept")
}

func TestUninstallClaudeCustomHome(t *testing.T) {
	e := newUnEnv(t)
	home := e.claudeHome(t, ".claude-work")
	e.state(t, HomeState{Path: home, Type: TypeClaude})
	e.answer("y", "n")
	e.run(t)
	assert.Equal(t, []string{"CLAUDE_CONFIG_DIR=" + home + " claude mcp remove -s user slack"}, e.r.calls)
	_, err := os.Lstat(e.bin)
	assert.NoError(t, err, "link kept when declined")
}

func TestUninstallMissingCLIGivesCommand(t *testing.T) {
	e := newUnEnv(t)
	home := e.claudeHome(t, ".claude-work")
	e.state(t, HomeState{Path: home, Type: TypeClaude})
	e.r.missing = map[string]bool{"claude": true}
	e.answer("y", "n")
	res := e.run(t)
	assert.Empty(t, e.r.calls)
	require.Len(t, res[0].Manual, 1)
	assert.Equal(t, "CLAUDE_CONFIG_DIR="+home+" claude mcp remove -s user slack", res[0].Manual[0])
}

func TestUninstallInvalidSettingsRefused(t *testing.T) {
	e := newUnEnv(t)
	home := e.claudeHome(t, ".claude-work")
	bad := filepath.Join(home, "settings.json")
	require.NoError(t, os.WriteFile(bad, []byte("{nope"), 0o600))
	e.state(t, HomeState{Path: home, Type: TypeClaude})
	e.answer("y")
	res := e.run(t)
	got, _ := os.ReadFile(bad)
	assert.Equal(t, "{nope", string(got))
	assert.Contains(t, strings.Join(res[0].Notes, "\n"), "not valid JSON")
	assert.Len(t, e.loadState(t).Homes, 1, "kept in state so it can be retried")
	assert.False(t, exists(EnvPath(home)), "other steps still ran")
}

func TestUninstallCodexYes(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	plist, target := e.launchAgent(t, home)
	script := e.startScript(t, home)
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "y", "y", "y")
	res := e.run(t)

	doc, _ := json.Marshal(readJSON(t, filepath.Join(home, "hooks.json")))
	assert.Contains(t, string(doc), "other-stop")
	assert.NotContains(t, string(doc), "stop-hook")
	assert.NotContains(t, string(doc), "PermissionRequest")

	rules, _ := os.ReadFile(filepath.Join(home, "rules", "default.rules"))
	assert.Contains(t, string(rules), `["git", "status"]`)
	assert.NotContains(t, string(rules), `"chat"`)

	assert.Contains(t, e.r.calls, "CODEX_HOME="+home+" codex mcp remove slack")
	assert.Contains(t, e.r.calls, "launchctl bootout "+target)
	assert.False(t, exists(plist))
	assert.False(t, exists(script))
	// backups go to the repo, not where launchd or PATH would find them
	assert.Empty(t, baks(t, plist), "no backup in LaunchAgents")
	assert.Empty(t, baks(t, script), "no backup next to the script")
	for _, orig := range []string{plist, script} {
		m := baks(t, filepath.Join(e.repo, ".install", "backups", filepath.Base(orig)))
		require.Len(t, m, 1)
		info, err := os.Stat(m[0])
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	assert.Contains(t, strings.Join(res[0].Notes, "\n"), filepath.Join(e.repo, ".install", "backups"))
	assert.False(t, exists(EnvPath(home)))
	assert.False(t, exists(filepath.Join(home, "skills", "slack-agent-chat", "SKILL.md")))
}

func TestUninstallCodexNoKeepsAgentAndScript(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	plist, _ := e.launchAgent(t, home)
	script := e.startScript(t, home)
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "n", "n", "n")
	e.run(t)
	assert.True(t, exists(plist))
	assert.True(t, exists(script))
	for _, c := range e.r.calls {
		assert.NotContains(t, c, "bootout")
	}
}

func TestUninstallCodexAgentNotLoadedNoPlistIsSilent(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	e.r.scripts = map[string]fakeResult{"launchctl print": {err: errors.New("not loaded")}}
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "n") // home, link
	res := e.run(t)
	assert.Contains(t, e.p.Out.String(), "Uninstall slack-mcp-server from")
	assert.NotContains(t, e.p.Out.String(), "launch agent")
	assert.NotContains(t, strings.Join(res[0].Notes, "\n"), "launch agent")
}

func TestUninstallBootoutNotLoadedIgnored(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	plist, _ := e.launchAgent(t, home)
	e.r.scripts = map[string]fakeResult{"launchctl bootout": {out: "Boot-out failed: 3: No such process", err: errors.New("exit 3")}}
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "y", "n", "n")
	res := e.run(t)
	assert.False(t, exists(plist))
	assert.NotContains(t, strings.Join(res[0].Notes, "\n"), "FAILED")
}

func TestUninstallCodexConfigApproveLine(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	cfg := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("model = \"x\"\n\n[mcp_servers.slack]\ncommand = \"b\"\ndefault_tools_approval_mode = \"approve\"\n\n[other]\ndefault_tools_approval_mode = \"approve\"\n"), 0o600))
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "n", "n", "n")
	e.run(t)
	got, _ := os.ReadFile(cfg)
	assert.Equal(t, "model = \"x\"\n\n[mcp_servers.slack]\ncommand = \"b\"\n\n[other]\ndefault_tools_approval_mode = \"approve\"\n", string(got))
	assert.NotEmpty(t, baks(t, cfg))
}

func TestUninstallMissingHomeFolder(t *testing.T) {
	e := newUnEnv(t)
	home := filepath.Join(e.user, ".codex-test") // never created
	plist, target := e.launchAgent(t, home)
	script := e.startScript(t, home)
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "y", "y", "n")
	res := e.run(t)

	assert.Contains(t, e.p.Out.String(), "(folder missing)")
	assert.Equal(t, []string{"launchctl bootout " + target}, e.r.calls, "no codex mcp remove")
	assert.False(t, exists(plist))
	assert.False(t, exists(script))
	assert.False(t, exists(home), "home not recreated")
	assert.Contains(t, strings.Join(res[0].Notes, "\n"), "folder missing")
	assert.Empty(t, e.loadState(t).Homes)
}

func TestUninstallMissingDefaultClaudeHomeStillUnregisters(t *testing.T) {
	e := newUnEnv(t)
	home := filepath.Join(e.user, ".claude")
	e.state(t, HomeState{Path: home, Type: TypeClaude})
	e.answer("y", "n")
	e.run(t)
	assert.Equal(t, []string{"-u CLAUDE_CONFIG_DIR claude mcp remove -s user slack"}, e.r.calls)
}

func TestUninstallFindsStandardHomesNotInState(t *testing.T) {
	e := newUnEnv(t)
	e.claudeHome(t, ".claude")
	require.NoError(t, os.MkdirAll(filepath.Join(e.user, ".codex"), 0o700)) // not ours
	e.state(t)
	e.answer("n")
	e.run(t)
	out := e.p.Out.String()
	assert.Contains(t, out, filepath.Join(e.user, ".claude"))
	assert.NotContains(t, out, filepath.Join(e.user, ".codex"))
}

func TestUninstallLinkOnlyWhenNoHomesRemainAndSymlink(t *testing.T) {
	e := newUnEnv(t)
	a := e.claudeHome(t, ".claude-a")
	b := e.claudeHome(t, ".claude-b")
	e.state(t, HomeState{Path: a, Type: TypeClaude}, HomeState{Path: b, Type: TypeClaude})
	e.answer("y", "n") // a yes, b no: no link question
	e.run(t)
	_, err := os.Lstat(e.bin)
	assert.NoError(t, err)
	assert.Len(t, e.loadState(t).Homes, 1)

	// a regular file is never removed
	e2 := newUnEnv(t)
	require.NoError(t, os.Remove(e2.bin))
	require.NoError(t, os.WriteFile(e2.bin, []byte("real"), 0o755))
	c := e2.claudeHome(t, ".claude-c")
	e2.state(t, HomeState{Path: c, Type: TypeClaude})
	e2.answer("y")
	e2.run(t)
	assert.True(t, exists(e2.bin))
}

func TestUninstallSummaryReminders(t *testing.T) {
	e := newUnEnv(t)
	home := e.claudeHome(t, ".claude-a")
	e.state(t, HomeState{Path: home, Type: TypeClaude, Bot: "claude-a"})
	e.answer("y", "n")
	res := e.run(t)
	var sb strings.Builder
	printUninstallSummary(&sb, res)
	out := sb.String()
	assert.Contains(t, out, "https://api.slack.com/apps")
	assert.Contains(t, out, "claude-a")
	assert.Contains(t, out, "Restart any running Claude/Codex sessions")
}

func TestUninstallClaudeMissingFolderNoteIsKindSpecific(t *testing.T) {
	e := newUnEnv(t)
	home := filepath.Join(e.user, ".claude-gone")
	e.state(t, HomeState{Path: home, Type: TypeClaude})
	e.answer("y", "n")
	res := e.run(t)
	n := strings.Join(res[0].Notes, "\n")
	assert.Contains(t, n, "folder missing")
	assert.NotContains(t, n, "rule")
}

func TestUninstallRulesReadErrorFails(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	rules := filepath.Join(home, "rules", "default.rules")
	require.NoError(t, os.Remove(rules))
	require.NoError(t, os.Mkdir(rules, 0o700)) // reading a directory fails
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "n", "n", "n")
	res := e.run(t)
	assert.Contains(t, strings.Join(res[0].Notes, "\n"), "FAILED: rule")
	assert.Len(t, e.loadState(t).Homes, 1)
}
