package agentchat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveEnvFile(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{"flag wins", "~/x/f.env", map[string]string{"HOME": "/h", "CODEX_THREAD_ID": "t"}, "/h/x/f.env"},
		{"codex default home", "", map[string]string{"HOME": "/h", "CODEX_THREAD_ID": "t"}, "/h/.codex/slack-mcp-server.env"},
		{"codex explicit home", "", map[string]string{"HOME": "/h", "CODEX_THREAD_ID": "t", "CODEX_HOME": "~/.codex-rezilient"}, "/h/.codex-rezilient/slack-mcp-server.env"},
		{"claude default", "", map[string]string{"HOME": "/h", "CLAUDECODE": "1"}, "/h/.claude/slack-mcp-server.env"},
		{"claude config dir", "", map[string]string{"HOME": "/h", "CLAUDECODE": "1", "CLAUDE_CONFIG_DIR": "/c"}, "/c/slack-mcp-server.env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveEnvFile(c.flag, envMap(c.env))
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
	_, err := ResolveEnvFile("", envMap(map[string]string{"HOME": "/h"}))
	assert.Error(t, err)
}

func TestLoadEnvFileReplacesInheritedSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, EnvFileName)
	require.NoError(t, os.WriteFile(path, []byte("SLACK_MCP_XOXB_TOKEN=xoxb-file\nSLACK_MCP_ADD_MESSAGE_TOOL=true\n"), 0o600))
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "xoxb-shell")
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "xoxp-shell")

	require.NoError(t, LoadEnvFile(path))

	assert.Equal(t, "xoxb-file", os.Getenv("SLACK_MCP_XOXB_TOKEN"))
	assert.Equal(t, "true", os.Getenv("SLACK_MCP_ADD_MESSAGE_TOOL"))
	_, inherited := os.LookupEnv("SLACK_MCP_XOXP_TOKEN")
	assert.False(t, inherited, "inherited SLACK_MCP_* must be cleared")
	os.Unsetenv("SLACK_MCP_ADD_MESSAGE_TOOL")
}

func TestLoadEnvFileMissing(t *testing.T) {
	err := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	assert.ErrorContains(t, err, "nope.env")
}

func TestNewHome(t *testing.T) {
	h := NewHome("/h/.codex/slack-mcp-server.env")
	assert.Equal(t, "/h/.codex", h.Dir)
	assert.Equal(t, "/h/.codex/slack-agent-chat", h.StateDir)
	assert.Equal(t, "/h/.codex/slack-agent-chat/listener.sock", h.ControlSocket)
	assert.Equal(t, "/h/.codex/slack-agent-chat/state.json", h.StateFile)
	assert.Equal(t, "/h/.codex/slack-agent-chat/listener.log", h.LogFile)
	assert.Equal(t, "/h/.codex/app-server-control/app-server-control.sock", h.CodexSocket)
}

// The env file holds tokens and configures a long-lived process: it must be
// private to this user and may set only SLACK_MCP_* settings.
func TestLoadEnvFileRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.env")
	require.NoError(t, os.WriteFile(open, []byte("SLACK_MCP_XOXB_TOKEN=x\n"), 0o600))
	require.NoError(t, os.Chmod(open, 0o644))
	assert.ErrorContains(t, LoadEnvFile(open), "accessible to other users (mode 0644)")

	other := filepath.Join(dir, "other.env")
	require.NoError(t, os.WriteFile(other, []byte("SLACK_MCP_XOXB_TOKEN=x\nPATH=/tmp/evil\n"), 0o600))
	assert.ErrorContains(t, LoadEnvFile(other), "sets PATH; only SLACK_MCP_* settings are allowed")
}
