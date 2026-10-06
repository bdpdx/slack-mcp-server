package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdpdx/slack-mcp-server/pkg/agentchat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateSlackEnv restores every SLACK_MCP_* variable after the test, since
// agentchat.LoadEnvFile changes them process-wide.
func isolateSlackEnv(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "SLACK_MCP_") {
			saved[k] = v
		}
	}
	t.Cleanup(func() {
		for _, kv := range os.Environ() {
			if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "SLACK_MCP_") {
				os.Unsetenv(k)
			}
		}
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

func TestEnvRoundTrip(t *testing.T) {
	isolateSlackEnv(t)
	existing := map[string]string{
		"SLACK_MCP_XOXB_TOKEN": "xoxb-old", "SLACK_MCP_JOIN_TOOL": "false",
		"SLACK_MCP_FILES_DIR": "~/Elsewhere", "PATH": "/evil",
	}
	tools := DefaultTools(existing)
	assert.False(t, tools["SLACK_MCP_JOIN_TOOL"], "an existing choice wins")
	assert.True(t, tools["SLACK_MCP_ADD_MESSAGE_TOOL"], "missing tools default on")
	assert.False(t, tools["SLACK_MCP_INVITE_SHARED_TOOL"])

	values := MergeEnv(existing, Tokens{Bot: "xoxb-new", User: "xoxp-u", App: "xapp-a"}, tools)
	data, err := RenderEnv(values)
	require.NoError(t, err)
	text := string(data)
	assert.Contains(t, text, "SLACK_MCP_XOXB_TOKEN=xoxb-new\n")
	assert.Contains(t, text, "SLACK_MCP_FILES_DIR=~/Elsewhere\n", "other settings are kept")
	assert.Contains(t, text, "SLACK_MCP_JOIN_TOOL=false\n")
	assert.NotContains(t, text, "PATH=", "only SLACK_MCP_* keys")
	assert.True(t, strings.HasPrefix(text, "# slack-mcp-server settings"))

	p := filepath.Join(t.TempDir(), "slack-mcp-server.env")
	require.NoError(t, writeAtomic(p, data, 0o600))
	require.NoError(t, agentchat.LoadEnvFile(p), "the server accepts the file")
	assert.Equal(t, "xapp-a", os.Getenv("SLACK_MCP_XAPP_TOKEN"))

	got, err := ReadEnv(p)
	require.NoError(t, err)
	assert.Equal(t, "xoxp-u", got["SLACK_MCP_XOXP_TOKEN"])
	empty, err := ReadEnv(filepath.Join(t.TempDir(), "none"))
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestCustomToolValuesAreKept(t *testing.T) {
	existing := map[string]string{
		"SLACK_MCP_ADD_MESSAGE_TOOL":    "C123,#general",
		"SLACK_MCP_DELETE_MESSAGE_TOOL": "!C123",
		"SLACK_MCP_REACTION_TOOL":       "C9",
		"SLACK_MCP_JOIN_TOOL":           "no",
	}
	assert.Equal(t, map[string]string{
		"SLACK_MCP_ADD_MESSAGE_TOOL":    "C123,#general",
		"SLACK_MCP_DELETE_MESSAGE_TOOL": "!C123",
		"SLACK_MCP_REACTION_TOOL":       "C9",
	}, CustomTools(existing))
	tools := DefaultTools(existing)
	assert.NotContains(t, tools, "SLACK_MCP_ADD_MESSAGE_TOOL", "custom values are not toggled")
	assert.NotContains(t, tools, "SLACK_MCP_DELETE_MESSAGE_TOOL")
	assert.False(t, tools["SLACK_MCP_JOIN_TOOL"])
	merged := MergeEnv(existing, Tokens{Bot: "xoxb-1", User: "xoxp-1", App: "xapp-1"}, tools)
	assert.Equal(t, "C123,#general", merged["SLACK_MCP_ADD_MESSAGE_TOOL"])
	assert.Equal(t, "!C123", merged["SLACK_MCP_DELETE_MESSAGE_TOOL"])
}

func TestRenderEnvQuotesValuesThatNeedIt(t *testing.T) {
	isolateSlackEnv(t)
	values := map[string]string{
		"SLACK_MCP_XOXB_TOKEN":       "xoxb-1-2",
		"SLACK_MCP_ADD_MESSAGE_TOOL": "C123,#general",
		"SLACK_MCP_A":                "a #x",
		"SLACK_MCP_B":                "$HOME/files",
		"SLACK_MCP_C":                `say "hi" now`,
		"SLACK_MCP_D":                "it's $HOME",
		"SLACK_MCP_E":                "line1\nline2",
		"SLACK_MCP_F":                `back\slash`,
		"SLACK_MCP_G":                "",
		"SLACK_MCP_H":                "  padded  ",
	}
	data, err := RenderEnv(values)
	require.NoError(t, err)
	assert.Contains(t, string(data), "SLACK_MCP_XOXB_TOKEN=xoxb-1-2\n", "tokens stay plain")

	p := filepath.Join(t.TempDir(), "slack-mcp-server.env")
	require.NoError(t, writeAtomic(p, data, 0o600))
	got, err := ReadEnv(p)
	require.NoError(t, err)
	assert.Equal(t, values, got)
	require.NoError(t, agentchat.LoadEnvFile(p))
	for k, v := range values {
		assert.Equal(t, v, os.Getenv(k), k)
	}
}
