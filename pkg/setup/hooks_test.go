package setup

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookCommandQuotesSpaces(t *testing.T) {
	cmd := HookCommand("/Users/a/My Tools/slack-mcp-server", "/Users/a/.claude/slack-mcp-server.env", "stop-hook")
	assert.Equal(t, `'/Users/a/My Tools/slack-mcp-server' chat --env-file /Users/a/.claude/slack-mcp-server.env stop-hook`, cmd)
	bin, ok := ourBin(cmd)
	assert.True(t, ok)
	assert.Equal(t, "/Users/a/My Tools/slack-mcp-server", bin)
}

func TestMergeHooksReplacesOursKeepsOthers(t *testing.T) {
	var events map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
	  "UserPromptSubmit": [{"hooks": [{"type":"command","command":"/old/slack-mcp-server chat --env-file /h/slack-mcp-server.env relay-hook","timeout":15}]}],
	  "PermissionRequest": [{"hooks": [{"type":"command","command":"/old/slack-mcp-server chat --env-file /h/slack-mcp-server.env approval-hook","timeout":660}]}],
	  "Stop": [{"hooks": [{"type":"command","command":"say done"}]}]
	}`), &events))

	out := MergeHooks(events, CodexHookSpecs(false), "/new/slack-mcp-server", "/h/slack-mcp-server.env", "SlackAgentChat")
	data, _ := json.Marshal(out)
	s := string(data)
	assert.NotContains(t, s, "/old/", "old paths are replaced")
	assert.NotContains(t, s, "approval-hook", "an unwanted approval-hook is removed")
	assert.Contains(t, s, "say done", "other hooks are kept")
	assert.Contains(t, s, `"statusMessage":"SlackAgentChat"`)
	assert.Equal(t, 1, countCommands(out, "relay-hook"))
	assert.Equal(t, 1, countCommands(out, "stop-hook"))

	again := MergeHooks(out, CodexHookSpecs(false), "/new/slack-mcp-server", "/h/slack-mcp-server.env", "SlackAgentChat")
	assert.Equal(t, 1, countCommands(again, "relay-hook"), "re-running does not duplicate")
}

func TestClaudeHookSpecs(t *testing.T) {
	specs := ClaudeHookSpecs()
	byHook := map[string]HookSpec{}
	for _, s := range specs {
		byHook[s.Hook] = s
	}
	assert.Equal(t, 660, byHook["approval-hook"].Timeout)
	assert.Equal(t, "AskUserQuestion", byHook["ask-hook"].Matcher)
	assert.Equal(t, "PreToolUse", byHook["ask-hook"].Event)
	assert.Len(t, specs, 4)
	assert.Len(t, CodexHookSpecs(true), 3)
}

func countCommands(events map[string]any, hook string) int {
	n := 0
	for _, list := range events {
		for _, e := range list.([]any) {
			for _, h := range e.(map[string]any)["hooks"].([]any) {
				if args := splitCommand(h.(map[string]any)["command"].(string)); len(args) > 0 && args[len(args)-1] == hook {
					n++
				}
			}
		}
	}
	return n
}

func TestMergeHooksReplacesHookWithTrailingArgs(t *testing.T) {
	var events map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
	  "PermissionRequest": [{"hooks": [{"type":"command","command":"/old/slack-mcp-server chat --env-file /h/slack-mcp-server.env approval-hook --wait 5m","timeout":400}]}]
	}`), &events))
	out := MergeHooks(events, ClaudeHookSpecs(), "/new/slack-mcp-server", "/h/slack-mcp-server.env", "")
	data, _ := json.Marshal(out)
	assert.NotContains(t, string(data), "--wait 5m", "the old hook is replaced")
	assert.Equal(t, 1, countCommands(out, "approval-hook"))
	assert.False(t, isOurHook("/x/slack-mcp-server serve approval-hook"), "only chat subcommands")
}
