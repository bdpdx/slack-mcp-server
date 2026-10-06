package setup

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bdpdx/slack-mcp-server/pkg/toolconfig"
	"github.com/joho/godotenv"
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

// CustomTools returns the existing *_TOOL settings that are not plain
// booleans (e.g. channel lists like "C123,#general" or "!C123"). Setup keeps
// them verbatim and does not offer to toggle them.
func CustomTools(existing map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		if !strings.HasPrefix(k, "SLACK_MCP_") || !strings.HasSuffix(k, "_TOOL") {
			continue
		}
		if _, ok := toolconfig.ParseBool(v); !ok {
			out[k] = v
		}
	}
	return out
}

// DefaultTools proposes on/off tool settings: existing values win, else
// defaults. Custom (non-boolean) values are left out; MergeEnv keeps them.
func DefaultTools(existing map[string]string) map[string]bool {
	custom := CustomTools(existing)
	tools := map[string]bool{}
	for _, k := range DefaultOnTools {
		tools[k] = true
	}
	for _, k := range DefaultOffTools {
		tools[k] = false
	}
	for k := range tools {
		if _, ok := custom[k]; ok {
			delete(tools, k)
		} else if v, ok := existing[k]; ok {
			tools[k], _ = toolconfig.ParseBool(v)
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

// RenderEnv writes sorted KEY=value lines (SLACK_MCP_* only), quoting a
// value only when godotenv would not read it back unchanged.
func RenderEnv(values map[string]string) ([]byte, error) {
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
		v, err := envValue(k, values[k])
		if err != nil {
			return nil, err
		}
		b.WriteString(k + "=" + v + "\n")
	}
	return []byte(b.String()), nil
}

// envValue returns the first form of v (plain, single-quoted, double-quoted
// with escapes) that godotenv parses back to exactly v.
func envValue(k, v string) (string, error) {
	dq := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, `"`, `\"`, "$", `\$`).Replace(v)
	for _, form := range []string{v, "'" + v + "'", `"` + dq + `"`} {
		if got, err := godotenv.Unmarshal(k + "=" + form); err == nil && len(got) == 1 && got[k] == v {
			return form, nil
		}
	}
	return "", fmt.Errorf("the value of %s cannot be written to an env file; change it and run setup again", k)
}
