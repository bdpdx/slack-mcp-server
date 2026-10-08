package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"

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
	Reason               string          `json:"reason"`    // PermissionDenied: the classifier's reason
	PromptID             string          `json:"prompt_id"` // Claude Code
	TurnID               string          `json:"turn_id"`   // Codex
	// PermissionSuggestions (Claude Code PermissionRequest): the rules the
	// terminal dialog would offer to save.
	PermissionSuggestions json.RawMessage `json:"permission_suggestions,omitempty"`
}

// codex reports whether the event comes from Codex, which takes no
// permission rules back from hooks.
func (ev hookEvent) codex() bool { return ev.TurnID != "" && ev.PromptID == "" }

// isQuietHook reports whether cmd is a hook that must never fail or block
// its host: any error leaves things to the host's normal handling.
func isQuietHook(cmd string) bool {
	return cmd == "ask-hook" || cmd == "approval-hook" || cmd == "denied-hook" || cmd == "stop-hook"
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

// maxApprovalDetail keeps the message within Slack's 3,000-character limit
// for a section's text.
const maxApprovalDetail = 2500

// revealHidden replaces characters that render invisibly or reorder text
// (format characters such as zero-width spaces, bidi overrides and Unicode
// tags, and control characters other than newline and tab) with a visible
// ⟨U+XXXX⟩, reporting whether there were any.
func revealHidden(s string) (string, bool) {
	var b strings.Builder
	hidden := false
	for _, r := range s {
		if r != '\n' && r != '\t' && (unicode.Is(unicode.Cf, r) || unicode.IsControl(r) || r == '\u2028' || r == '\u2029') {
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
			hidden = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), hidden
}

// FormatApproval renders a pending permission prompt as a Slack message.
// unsafe is non-empty when the message cannot show the owner exactly what
// they would approve: the detail had to be cut, or it holds characters that
// render invisibly or reorder text. Such a request must not be allowed from
// Slack; unsafe says why.
func FormatApproval(ownerID, agent, tool string, input json.RawMessage) (msg, unsafe string) {
	detail, unsafe := toolDetail(input)
	msg = fmt.Sprintf("<@%s> %s needs your approval: %s", ownerID, slackEscaper.Replace(agent), slackEscaper.Replace(tool))
	return msg + detail, unsafe
}

// toolDetail renders a tool call's input as a code block for an approval
// message ("" when there is none), with the reason it cannot be shown
// exactly, if any (see FormatApproval).
func toolDetail(input json.RawMessage) (block, unsafe string) {
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
	detail, hidden := revealHidden(detail)
	if hidden {
		unsafe = "it contains invisible or text-reordering characters (shown as ⟨U+…⟩)"
	}
	if r := []rune(detail); len(r) > maxApprovalDetail {
		unsafe = fmt.Sprintf("it is too long to show in full (%d characters; the first %d are shown)", len(r), maxApprovalDetail)
		detail = string(r[:maxApprovalDetail]) + "…"
	}
	if detail == "" {
		return "", unsafe
	}
	// Inside a code block only the backticks that would close it matter.
	return "\n```" + strings.ReplaceAll(slackEscaper.Replace(detail), "```", "`\u200b``") + "```", unsafe
}

// askHookReason is shown both to the owner in the terminal (Claude Code
// labels it a hook error) and to Claude, so it opens with a line for the
// owner and then tells Claude what to do.
func askHookReason(posted []postedMsg) string {
	var names, where []string
	for _, p := range posted {
		names = append(names, "#"+p.name)
		where = append(where, fmt.Sprintf("#%s (ts %s)", p.name, p.ts))
	}
	return fmt.Sprintf("Question sent to Slack: %s. Answer it there.\n"+
		"(For the agent: the questions were posted to %s instead of the terminal. Do not ask again. "+
		"Keep working on anything that does not depend on the answers; if nothing is left, end your turn. "+
		"The answer will arrive as a [slack-agent-chat] notice from one of those channels, even mid-turn.)",
		strings.Join(names, ", "), strings.Join(where, ", "))
}

// askHook (Claude PreToolUse on AskUserQuestion) posts the questions to the
// session's direct channel and denies the terminal question, which would
// block the session so that a Slack answer could not be processed. A
// session watching no project, or any failure, leaves the question to the
// terminal.
func (c *cli) askHook(ctx context.Context, ev hookEvent) int {
	targets, me, err := c.directChannels(ctx, c.hookSession(ev))
	if err != nil {
		return 0
	}
	text, ok := FormatQuestions(me.ownerID, me.agentName, ev.ToolInput)
	if !ok {
		return 0
	}
	posted := c.postEach(ctx, targets, "question", slack.MsgOptionText(text, false))
	if len(posted) == 0 {
		return 0
	}
	c.printJSON(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": askHookReason(posted),
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

// namedChannel is a channel ID with its name.
type namedChannel struct{ id, name string }

// postedMsg is a message a hook posted: its channel, the channel's name and
// its ts.
type postedMsg struct{ channel, name, ts string }

// directChannels returns the #PROJECT__OWNER_AGENT channels session watches,
// one per watched project, in name order. A session watching no project
// gets an error.
func (c *cli) directChannels(ctx context.Context, session string) ([]namedChannel, identity, error) {
	ids, names, err := c.sessionChannels(ctx, session)
	if err != nil {
		return nil, identity{}, err
	}
	me, err := c.identity(ctx)
	if err != nil {
		return nil, identity{}, err
	}
	found := directChannelsOf(ids, names, me.ownerName, me.agentName)
	if len(found) == 0 {
		return nil, identity{}, errors.New("this session watches no direct channel")
	}
	return found, me, nil
}

// directChannelsOf picks, from the watched channels ids (named by names),
// the direct channel of each watched project, in name order.
func directChannelsOf(ids []string, names map[string]string, owner, agent string) []namedChannel {
	var found []namedChannel
	for _, id := range ids {
		project, derived := ProjectOf(names[id])
		if derived {
			continue
		}
		name, err := DirectChannelName(project, owner, agent)
		if err != nil {
			continue
		}
		for _, d := range ids {
			if names[d] == name {
				found = append(found, namedChannel{d, name})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	return found
}

// postEach posts the same message (what names it in errors) to every target,
// returning the copies that were posted.
func (c *cli) postEach(ctx context.Context, targets []namedChannel, what string, opts ...slack.MsgOption) []postedMsg {
	var posted []postedMsg
	for _, t := range targets {
		_, ts, err := c.bot.PostMessageContext(ctx, t.id, opts...)
		if err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: posting %s to #%s: %v\n", what, t.name, err)
			continue
		}
		posted = append(posted, postedMsg{t.id, t.name, ts})
	}
	return posted
}
