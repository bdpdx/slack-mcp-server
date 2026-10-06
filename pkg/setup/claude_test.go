package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	missing map[string]bool
	calls   []string
	// scripts maps a "name args..." prefix (env ignored) to the result of a
	// matching Run; the longest matching prefix wins.
	scripts map[string]fakeResult
}

type fakeResult struct {
	out string
	err error
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.missing[name] {
		return "", errors.New("not found")
	}
	return "/usr/local/bin/" + name, nil
}

// Run records the call; an env entry without '=' (unset) shows as -u NAME.
func (f *fakeRunner) Run(env []string, name string, args ...string) (string, error) {
	var parts []string
	for _, e := range env {
		if !strings.Contains(e, "=") {
			e = "-u " + e
		}
		parts = append(parts, e)
	}
	f.calls = append(f.calls, strings.TrimSpace(strings.Join(parts, " ")+" "+name+" "+strings.Join(args, " ")))
	cmd := name + " " + strings.Join(args, " ")
	best := ""
	var res fakeResult
	for prefix, v := range f.scripts {
		if strings.HasPrefix(cmd, prefix) && len(prefix) > len(best) {
			best, res = prefix, v
		}
	}
	return res.out, res.err
}

func TestInstallClaude(t *testing.T) {
	user := t.TempDir()
	home := filepath.Join(user, ".claude") // the default home: no CLAUDE_CONFIG_DIR
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"),
		[]byte(`{"model":"x","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`), 0o600))
	r := &fakeRunner{}
	res, err := InstallClaude(home, user, "/bin dir/slack-mcp-server", r, testNow)
	require.NoError(t, err)

	skill, err := os.ReadFile(filepath.Join(home, "skills", "slack-agent-chat", "SKILL.md"))
	require.NoError(t, err)
	assert.Contains(t, string(skill), "/bin dir/slack-mcp-server chat")
	assert.NotContains(t, string(skill), "@BIN@")
	assert.FileExists(t, filepath.Join(home, "skills", "slack-agent-chat", "COLLABORATION.md"))

	var doc map[string]any
	data, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	require.NoError(t, json.Unmarshal(data, &doc))
	assert.Equal(t, "x", doc["model"], "other settings kept")
	s := string(data)
	assert.Contains(t, s, "say done")
	assert.Contains(t, s, `'/bin dir/slack-mcp-server' chat --env-file`)
	assert.Contains(t, s, `"timeout": 660`)
	assert.FileExists(t, filepath.Join(home, "settings.json.bak-20261005120000"))

	require.Len(t, r.calls, 2)
	assert.Equal(t, "-u CLAUDE_CONFIG_DIR claude mcp remove -s user slack", r.calls[0], "an inherited CLAUDE_CONFIG_DIR is dropped")
	assert.Contains(t, r.calls[1], "-u CLAUDE_CONFIG_DIR claude mcp add -s user slack -- /bin dir/slack-mcp-server --transport stdio --env-file "+filepath.Join(home, "slack-mcp-server.env"))
	assert.NotEmpty(t, res.Changed)
}

func TestInstallClaudeRefusesInvalidSettings(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"hooks": {`), 0o600))
	_, err := InstallClaude(home, t.TempDir(), "/b/slack-mcp-server", &fakeRunner{}, testNow)
	assert.ErrorContains(t, err, "not valid JSON")
	data, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	assert.Equal(t, `{"hooks": {`, string(data), "never overwritten")
}

func TestRegisterMCPWithoutCLI(t *testing.T) {
	manual, err := RegisterMCP(&fakeRunner{missing: map[string]bool{"claude": true}}, TypeClaude, "/h", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.Contains(t, manual, "claude mcp add -s user slack -- /b/slack-mcp-server --transport stdio --env-file /h/slack-mcp-server.env")
}

func TestRegisterMCPNonStandardClaudeHome(t *testing.T) {
	r := &fakeRunner{}
	_, err := RegisterMCP(r, TypeClaude, "/x/agent", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	require.Len(t, r.calls, 2)
	assert.Equal(t, "CLAUDE_CONFIG_DIR=/x/agent claude mcp remove -s user slack", r.calls[0])
	assert.Contains(t, r.calls[1], "CLAUDE_CONFIG_DIR=/x/agent claude mcp add -s user slack -- ")

	manual, err := RegisterMCP(&fakeRunner{missing: map[string]bool{"claude": true}}, TypeClaude, "/x/agent", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(manual, "CLAUDE_CONFIG_DIR=/x/agent claude mcp add"), manual)
}

func TestRegisterMCPManualQuotesEnvValue(t *testing.T) {
	manual, err := RegisterMCP(&fakeRunner{missing: map[string]bool{"claude": true}}, TypeClaude, "/a b/agent", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(manual, "CLAUDE_CONFIG_DIR='/a b/agent' claude mcp add"), manual)
	manual, err = RegisterMCP(&fakeRunner{missing: map[string]bool{"codex": true}}, TypeCodex, "/a b/.codex", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(manual, "CODEX_HOME='/a b/.codex' codex mcp add"), manual)
}

func TestInstallClaudeInvalidSettingsChangesNothing(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"hooks": []}`), 0o600))
	_, err := InstallClaude(home, t.TempDir(), "/b/slack-mcp-server", &fakeRunner{}, testNow)
	assert.ErrorContains(t, err, "settings.json")
	assert.ErrorContains(t, err, "not a JSON object")
	assert.NoDirExists(t, filepath.Join(home, "skills"))
	data, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	assert.Equal(t, `{"hooks": []}`, string(data))

	home2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home2, "settings.json"), []byte(`{`), 0o600))
	_, err = InstallClaude(home2, t.TempDir(), "/b/slack-mcp-server", &fakeRunner{}, testNow)
	assert.Error(t, err)
	assert.NoDirExists(t, filepath.Join(home2, "skills"))
}

func TestInstallClaudeNullSettings(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`null`), 0o600))
	_, err := InstallClaude(home, t.TempDir(), "/b/slack-mcp-server", &fakeRunner{}, testNow)
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	assert.Contains(t, string(data), `"hooks"`)
}

func TestInstallClaudeKeepsShellCharsUnescaped(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"),
		[]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"a && b 2>/dev/null <in"}]}]}}`), 0o600))
	_, err := InstallClaude(home, t.TempDir(), "/b/slack-mcp-server", &fakeRunner{}, testNow)
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	assert.Contains(t, string(data), `"command": "a && b 2>/dev/null <in"`)
	assert.NotContains(t, string(data), `\u0026`)
}

func TestRegisterMCPClaudeHomeTarget(t *testing.T) {
	r := &fakeRunner{}
	_, err := RegisterMCP(r, TypeClaude, "/x/.claude", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.Equal(t, "CLAUDE_CONFIG_DIR=/x/.claude claude mcp remove -s user slack", r.calls[0], "a .claude outside the user home is not the default")

	r = &fakeRunner{}
	_, err = RegisterMCP(r, TypeClaude, "/u/.claude/", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.Equal(t, "-u CLAUDE_CONFIG_DIR claude mcp remove -s user slack", r.calls[0])

	manual, err := RegisterMCP(&fakeRunner{missing: map[string]bool{"claude": true}}, TypeClaude, "/u/.claude", "/u", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(manual, "env -u CLAUDE_CONFIG_DIR claude mcp add"), manual)
}

func TestCommandEnv(t *testing.T) {
	base := []string{"PATH=/bin", "CODEX_HOME=/inherited", "CLAUDE_CONFIG_DIR=/inherited", "HOME=/u"}
	assert.Equal(t, []string{"PATH=/bin", "HOME=/u", "CODEX_HOME=/h"},
		commandEnv(base, []string{"CODEX_HOME=/h", "CLAUDE_CONFIG_DIR"}))
}
