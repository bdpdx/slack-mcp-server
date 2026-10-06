package agentchat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/slack-go/slack"
)

// hookEvent is the part of a PreToolUse, PermissionRequest or Stop hook
// event the Slack hooks read. Claude Code and Codex share these fields.
type hookEvent struct {
	SessionID            string          `json:"session_id"`
	Cwd                  string          `json:"cwd"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	PromptID             string          `json:"prompt_id"` // Claude Code
	TurnID               string          `json:"turn_id"`   // Codex
}

// isQuietHook reports whether cmd is a hook that must never fail or block
// its host: any error leaves things to the host's normal handling.
func isQuietHook(cmd string) bool {
	return cmd == "ask-hook" || cmd == "approval-hook" || cmd == "stop-hook"
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// FormatQuestions renders AskUserQuestion input as a Slack message to the
// owner. ok is false when the input holds no question.
func FormatQuestions(ownerID, agent string, input json.RawMessage) (string, bool) {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil || len(in.Questions) == 0 {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<@%s> %s has a question for you:\n", ownerID, slackEscaper.Replace(agent))
	for i, q := range in.Questions {
		b.WriteString("\n")
		fmt.Fprintf(&b, "*%d.", i+1)
		if q.Header != "" {
			fmt.Fprintf(&b, " %s", slackEscaper.Replace(q.Header))
		}
		fmt.Fprintf(&b, "* %s", slackEscaper.Replace(q.Question))
		if q.MultiSelect {
			b.WriteString(" _(pick any)_")
		}
		b.WriteString("\n")
		for j, o := range q.Options {
			fmt.Fprintf(&b, "    %c) %s", 'a'+rune(j%26), slackEscaper.Replace(o.Label))
			if o.Description != "" {
				fmt.Fprintf(&b, " — %s", slackEscaper.Replace(o.Description))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nReply here, e.g. \"1a\", or in your own words.")
	return b.String(), true
}

const maxApprovalDetail = 500

// FormatApproval renders a pending permission prompt as a Slack message.
func FormatApproval(ownerID, agent, tool string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	detail := ""
	switch v := in["command"].(type) {
	case string:
		detail = v
	case []any:
		parts := make([]string, len(v))
		for i, p := range v {
			parts[i] = fmt.Sprint(p)
		}
		detail = strings.Join(parts, " ")
	}
	if detail == "" {
		if d, ok := in["description"].(string); ok {
			detail = d
		} else if len(input) > 0 && string(input) != "null" {
			detail = string(input)
		}
	}
	if r := []rune(detail); len(r) > maxApprovalDetail {
		detail = string(r[:maxApprovalDetail]) + "…"
	}
	msg := fmt.Sprintf("<@%s> %s needs your approval: %s", ownerID, slackEscaper.Replace(agent), slackEscaper.Replace(tool))
	if detail != "" {
		// Inside a code block only the backticks that would close it matter.
		msg += "\n```" + strings.ReplaceAll(slackEscaper.Replace(detail), "```", "`​``") + "```"
	}
	return msg
}

// askHookReason tells Claude where its question went instead of the terminal.
func askHookReason(channelName, ts string) string {
	return fmt.Sprintf("The user is on Slack, so these questions were posted to #%s (ts %s) instead of being shown in the terminal. "+
		"Do not ask again. Keep working on anything that does not depend on the answers; if nothing is left, end your turn. "+
		"The user's answer will arrive as a [slack-agent-chat] notice from that channel, even mid-turn.", channelName, ts)
}

// askHook (Claude PreToolUse on AskUserQuestion) posts the questions to the
// session's direct channel and denies the terminal question, which would
// block the session so that a Slack answer could not be processed. A
// session watching no project, or any failure, leaves the question to the
// terminal.
func (c *cli) askHook(ctx context.Context, ev hookEvent) int {
	id, name, me, err := c.directChannel(ctx, c.hookSession(ev))
	if err != nil {
		return 0
	}
	text, ok := FormatQuestions(me.ownerID, me.agentName, ev.ToolInput)
	if !ok {
		return 0
	}
	_, ts, err := c.bot.PostMessageContext(ctx, id, slack.MsgOptionText(text, false))
	if err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: posting question to #%s: %v\n", name, err)
		return 0
	}
	c.printJSON(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": askHookReason(name, ts),
	}})
	return 0
}

func (c *cli) hookSession(ev hookEvent) string { return sessionOf(ev.SessionID) }

// sessionOf is the session a hook runs for: the event's session_id, or the
// Codex thread when the event has none. Subscriptions and turn marks use the
// same key.
func sessionOf(id string) string {
	if id != "" {
		return id
	}
	return os.Getenv("CODEX_THREAD_ID")
}

// directChannel returns the #PROJECT__OWNER_AGENT channel that session
// watches for its one project.
func (c *cli) directChannel(ctx context.Context, session string) (string, string, identity, error) {
	ids, names, err := c.sessionChannels(ctx, session)
	if err != nil {
		return "", "", identity{}, err
	}
	me, err := c.identity(ctx)
	if err != nil {
		return "", "", identity{}, err
	}
	var found []string // IDs of watched direct channels
	for _, id := range ids {
		project, derived := ProjectOf(names[id])
		if derived {
			continue
		}
		name, err := DirectChannelName(project, me.ownerName, me.agentName)
		if err != nil {
			continue
		}
		for _, d := range ids {
			if names[d] == name {
				found = append(found, d)
			}
		}
	}
	if len(found) != 1 {
		return "", "", identity{}, fmt.Errorf("this session watches %d direct channels", len(found))
	}
	return found[0], names[found[0]], me, nil
}
