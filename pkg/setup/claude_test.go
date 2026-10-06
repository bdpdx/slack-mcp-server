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
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.missing[name] {
		return "", errors.New("not found")
	}
	return "/usr/local/bin/" + name, nil
}

func (f *fakeRunner) Run(env []string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.TrimSpace(strings.Join(env, " ")+" "+name+" "+strings.Join(args, " ")))
	return "", nil
}

func TestInstallClaude(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".claude") // the standard name: no CLAUDE_CONFIG_DIR
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"),
		[]byte(`{"model":"x","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`), 0o600))
	r := &fakeRunner{}
	res, err := InstallClaude(home, "/bin dir/slack-mcp-server", r, testNow)
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
	assert.Equal(t, "claude mcp remove -s user slack", r.calls[0])
	assert.Contains(t, r.calls[1], "claude mcp add -s user slack -- /bin dir/slack-mcp-server --transport stdio --env-file "+filepath.Join(home, "slack-mcp-server.env"))
	assert.NotEmpty(t, res.Changed)
}

func TestInstallClaudeRefusesInvalidSettings(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"hooks": {`), 0o600))
	_, err := InstallClaude(home, "/b/slack-mcp-server", &fakeRunner{}, testNow)
	assert.ErrorContains(t, err, "not valid JSON")
	data, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	assert.Equal(t, `{"hooks": {`, string(data), "never overwritten")
}

func TestRegisterMCPWithoutCLI(t *testing.T) {
	manual, err := RegisterMCP(&fakeRunner{missing: map[string]bool{"claude": true}}, TypeClaude, "/h", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.Contains(t, manual, "claude mcp add -s user slack -- /b/slack-mcp-server --transport stdio --env-file /h/slack-mcp-server.env")
}

func TestRegisterMCPNonStandardClaudeHome(t *testing.T) {
	r := &fakeRunner{}
	_, err := RegisterMCP(r, TypeClaude, "/x/agent", "/b/slack-mcp-server")
	require.NoError(t, err)
	require.Len(t, r.calls, 2)
	assert.Equal(t, "CLAUDE_CONFIG_DIR=/x/agent claude mcp remove -s user slack", r.calls[0])
	assert.Contains(t, r.calls[1], "CLAUDE_CONFIG_DIR=/x/agent claude mcp add -s user slack -- ")

	manual, err := RegisterMCP(&fakeRunner{missing: map[string]bool{"claude": true}}, TypeClaude, "/x/agent", "/b/slack-mcp-server")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(manual, "CLAUDE_CONFIG_DIR=/x/agent claude mcp add"), manual)
}
