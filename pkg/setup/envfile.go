package setup

import (
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/joho/godotenv"
	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
)

// DefaultOnTools are enabled unless the user turns them off.
var DefaultOnTools = []string{
	"SLACK_MCP_ADD_MESSAGE_TOOL", "SLACK_MCP_JOIN_TOOL", "SLACK_MCP_USERGROUPS_WRITE_TOOL",
	"SLACK_MCP_RENAME_CHANNEL_TOOL", "SLACK_MCP_SET_TOPIC_TOOL", "SLACK_MCP_INVITE_TOOL",
	"SLACK_MCP_ATTACHMENT_TOOL", "SLACK_MCP_UPLOAD_FILE_TOOL",
}

// DefaultOffTools are offered but off by default.
var DefaultOffTools = []string{"SLACK_MCP_INVITE_SHARED_TOOL", "SLACK_MCP_DELETE_MESSAGE_TOOL"}

// Tokens are one agent's Slack tokens.
type Tokens struct{ Bot, User, App string }

// ReadEnv reads an env file; a missing file is empty.
func ReadEnv(path string) (map[string]string, error) {
	values, err := godotenv.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	return values, err
}

// DefaultTools proposes tool settings: existing values win, else defaults.
func DefaultTools(existing map[string]string) map[string]bool {
	tools := map[string]bool{}
	for _, k := range DefaultOnTools {
		tools[k] = true
	}
	for _, k := range DefaultOffTools {
		tools[k] = false
	}
	for k := range tools {
		if v, ok := existing[k]; ok {
			on, _ := toolconfig.ParseBool(v)
			tools[k] = on
		}
	}
	return tools
}

// MergeEnv keeps existing SLACK_MCP_* settings, replacing the tokens and
// tool settings.
func MergeEnv(existing map[string]string, tok Tokens, tools map[string]bool) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		if strings.HasPrefix(k, "SLACK_MCP_") {
			out[k] = v
		}
	}
	out["SLACK_MCP_XOXB_TOKEN"] = tok.Bot
	out["SLACK_MCP_XOXP_TOKEN"] = tok.User
	out["SLACK_MCP_XAPP_TOKEN"] = tok.App
	for k, on := range tools {
		out[k] = map[bool]string{true: "true", false: "false"}[on]
	}
	return out
}

// RenderEnv writes sorted KEY=value lines (SLACK_MCP_* only).
func RenderEnv(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for k := range values {
		if strings.HasPrefix(k, "SLACK_MCP_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# slack-mcp-server settings, written by `slack-mcp-server setup`. Keep private (mode 600).\n")
	for _, k := range keys {
		b.WriteString(k + "=" + values[k] + "\n")
	}
	return []byte(b.String())
}
