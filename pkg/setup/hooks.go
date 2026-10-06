package setup

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// HookSpec is one hook setup installs.
type HookSpec struct {
	Event   string
	Matcher string
	Hook    string
	Timeout int
}

var ourHooks = []string{"relay-hook", "ask-hook", "approval-hook", "stop-hook"}

// approvalTimeout must exceed approval-hook's --wait (10m) so the hook can
// hand the prompt back to the terminal before the host kills it.
const approvalTimeout = 660

// ClaudeHookSpecs are Claude Code's hooks.
func ClaudeHookSpecs() []HookSpec {
	return []HookSpec{
		{Event: "UserPromptSubmit", Hook: "relay-hook", Timeout: 15},
		{Event: "PreToolUse", Matcher: "AskUserQuestion", Hook: "ask-hook", Timeout: 15},
		{Event: "PermissionRequest", Hook: "approval-hook", Timeout: approvalTimeout},
		{Event: "Stop", Hook: "stop-hook", Timeout: 15},
	}
}

// CodexHookSpecs are Codex's hooks; approval is false for homes using
// auto_review, which runs after PermissionRequest hooks.
func CodexHookSpecs(approval bool) []HookSpec {
	specs := []HookSpec{
		{Event: "UserPromptSubmit", Hook: "relay-hook", Timeout: 15},
		{Event: "Stop", Hook: "stop-hook", Timeout: 15},
	}
	if approval {
		specs = append(specs, HookSpec{Event: "PermissionRequest", Hook: "approval-hook", Timeout: approvalTimeout})
	}
	return specs
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@-]+$`)

func shellQuote(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCommand is the shell command a host runs for hook.
func HookCommand(bin, envFile, hook string) string {
	return shellQuote(bin) + " chat --env-file " + shellQuote(envFile) + " " + hook
}

func isOurHook(cmd string) bool {
	if _, ok := ourBin(cmd); !ok {
		return false
	}
	args := splitCommand(cmd)
	return slices.Contains(ourHooks, args[len(args)-1])
}

// MergeHooks removes every slack-mcp-server chat hook from events (any
// binary path) and adds specs, keeping all other hooks.
func MergeHooks(events map[string]any, specs []HookSpec, bin, envFile, status string) map[string]any {
	out := map[string]any{}
	for event, list := range events {
		entries, _ := list.([]any)
		var kept []any
		for _, e := range entries {
			m, ok := e.(map[string]any)
			if !ok {
				kept = append(kept, e)
				continue
			}
			hs, _ := m["hooks"].([]any)
			var keepHooks []any
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if !isOurHook(cmd) {
					keepHooks = append(keepHooks, h)
				}
			}
			if len(keepHooks) > 0 {
				cp := map[string]any{}
				for k, v := range m {
					cp[k] = v
				}
				cp["hooks"] = keepHooks
				kept = append(kept, cp)
			}
		}
		if len(kept) > 0 {
			out[event] = kept
		}
	}
	for _, s := range specs {
		h := map[string]any{"type": "command", "command": HookCommand(bin, envFile, s.Hook), "timeout": s.Timeout}
		if status != "" {
			h["statusMessage"] = status
		}
		entry := map[string]any{"hooks": []any{h}}
		if s.Matcher != "" {
			entry["matcher"] = s.Matcher
		}
		list, _ := out[s.Event].([]any)
		out[s.Event] = append(list, entry)
	}
	return out
}

// ourBin returns the binary of a slack-mcp-server chat hook command.
func ourBin(cmd string) (string, bool) {
	args := splitCommand(cmd)
	if len(args) >= 2 && filepath.Base(args[0]) == "slack-mcp-server" && args[1] == "chat" {
		return args[0], true
	}
	return "", false
}

// splitCommand splits a command line written by HookCommand: words separated
// by spaces, single-quoted words kept whole.
func splitCommand(cmd string) []string {
	var args []string
	var cur strings.Builder
	inQuote, has := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\'':
			inQuote, has = !inQuote, true
		case c == '\\' && !inQuote && i+1 < len(cmd):
			i++
			cur.WriteByte(cmd[i])
			has = true
		case c == ' ' && !inQuote:
			if has {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if has {
		args = append(args, cur.String())
	}
	return args
}
