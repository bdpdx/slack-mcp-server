package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/agentchat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvRoundTrip(t *testing.T) {
	existing := map[string]string{
		"SLACK_MCP_XOXB_TOKEN": "xoxb-old", "SLACK_MCP_JOIN_TOOL": "false",
		"SLACK_MCP_FILES_DIR": "~/Elsewhere", "PATH": "/evil",
	}
	tools := DefaultTools(existing)
	assert.False(t, tools["SLACK_MCP_JOIN_TOOL"], "an existing choice wins")
	assert.True(t, tools["SLACK_MCP_ADD_MESSAGE_TOOL"], "missing tools default on")
	assert.False(t, tools["SLACK_MCP_INVITE_SHARED_TOOL"])

	values := MergeEnv(existing, Tokens{Bot: "xoxb-new", User: "xoxp-u", App: "xapp-a"}, tools)
	data := RenderEnv(values)
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
