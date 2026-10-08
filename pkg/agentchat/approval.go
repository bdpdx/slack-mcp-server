package agentchat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// approval-hook (PermissionRequest) asks the owner in the session's direct
// channel, with Allow / Deny / Answer-in-terminal buttons, and answers the
// host's permission prompt on their behalf. Claude Code has been observed to
// show its own terminal prompt at the same time, the first answer winning.
// However the hook finishes (answered, waited out, or stopped by its host),
// it redraws its request and tells the listener; a hook killed outright is
// caught by the listener once it stops polling. Unanswered, the hook waits a
// limited time and hands the prompt back to the terminal.
//
// Button clicks and replies in the message's thread reach this home's
// listener (the app's Socket Mode connection), which keeps the owner's
// answer for the hook to poll. Only a click can allow: Slack vouches for who
// clicked, but anything holding the owner's token, agents included, can post
// a reply as the owner. Replies can deny (with a reason) or pick the terminal.

const (
	approvalActionPrefix = "sac-approval-"
	decisionAllow        = "allow"
	decisionDeny         = "deny"
	decisionTerminal     = "terminal"
	decisionHint         = "hint"          // the owner typed an allow word; explain the button
	decisionAllowSimilar = "allow-similar" // allow and apply Claude Code's suggested rule
	decisionRuleSession  = "rule-session"  // allow and add the proposed rule for this session
	decisionRuleLocal    = "rule-local"    // ... for this project
	decisionRuleUser     = "rule-user"     // ... for all projects
	decisionPropose      = "propose"       // the owner typed a rule; draw its confirm buttons
	decisionRuleHelp     = "rule-help"     // the owner typed something after "allow" that is not a rule
	decisionTaken        = "taken"         // answered and handed to the hook
	decisionEnded        = "ended"         // the listener ended the request; nothing is decided
	defaultApprovalWait  = 10 * time.Minute
	approvalPoll         = time.Second
)

var (
	allowWords = map[string]bool{"yes": true, "y": true, "yep": true, "ok": true, "okay": true, "allow": true, "approve": true, "approved": true,
		"lgtm": true, "go": true, "sure": true, "+1": true, "👍": true, "white_check_mark": true, "✅": true, "heavy_check_mark": true, "✔️": true}
	denyWords = map[string]bool{"no": true, "n": true, "nope": true, "deny": true, "denied": true, "reject": true, "stop": true,
		"-1": true, "👎": true, "x": true, "❌": true}
	leadMentions = regexp.MustCompile(`^(?:\s*<@[^>]+>)+`)
	firstWord    = regexp.MustCompile(`^(\S+)\s*`)
)

// ParseApprovalReply reads an owner's thread reply to an approval request:
// it opens with an allow word, a deny word (anything after it is the reason
// for the agent), or "terminal". Any other reply denies, passing the whole
// reply to the agent as the reason. (The listener does not let a reply
// allow; see approvalReply.)
func ParseApprovalReply(text string) (decision, reason string) {
	decision, reason, _ = classifyApprovalReply(text)
	return decision, reason
}

// classifyApprovalReply is ParseApprovalReply that also reports whether the
// reply opened with one of the recognised words (explicit). A reply in the
// direct channel itself, rather than the request's thread, answers the
// request only when explicit; anything else there is an ordinary message.
func classifyApprovalReply(text string) (decision, reason string, explicit bool) {
	text = strings.TrimSpace(leadMentions.ReplaceAllString(text, ""))
	m := firstWord.FindStringSubmatch(text)
	if m == nil {
		return decisionDeny, "", false
	}
	word := strings.ToLower(strings.Trim(m[1], ".,!:;-—*_~`"))
	rest := strings.TrimSpace(strings.TrimLeft(text[len(m[0]):], ".,!:;-— "))
	switch {
	case allowWords[word]:
		return decisionAllow, "", true
	case denyWords[word]:
		return decisionDeny, rest, true
	case word == "terminal":
		return decisionTerminal, "", true
	}
	return decisionDeny, text, false
}

// approvalButton is a button answering request id with decision. Rule
// confirm buttons carry the rule's hash in their value ("id|hash").
func approvalButton(id, decision, label, style string) *slack.ButtonBlockElement {
	if len(label) > 75 {
		label = label[:72] + "…"
	}
	b := slack.NewButtonBlockElement(approvalActionPrefix+decision, id, slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
	if style != "" {
		b.Style = slack.Style(style)
	}
	return b
}

// approvalOptions are what a request offers besides Allow / Deny /
// terminal: Claude Code's suggested rule (similar, labelled similarLabel)
// and typed rules (rules), neither of which Codex supports.
type approvalOptions struct {
	similar      []json.RawMessage
	similarLabel string
	rules        bool
}

// approvalBlocks lays out the request. When unsafe is set (see
// FormatApproval) there is no Allow button: the owner can deny, or answer in
// the terminal, which shows the request in full.
func approvalBlocks(text, unsafe, id string, wait time.Duration, opt approvalOptions) []slack.Block {
	var buttons []slack.BlockElement
	note := "Or reply here: _no_ plus a reason for the agent, or _terminal_. Only the button can allow."
	if unsafe == "" {
		buttons = append(buttons, approvalButton(id, decisionAllow, "Allow", "primary"))
		if len(opt.similar) > 0 {
			buttons = append(buttons, approvalButton(id, decisionAllowSimilar, "Allow similar", ""))
			note = "_Allow similar_ also adds " + opt.similarLabel + ". " + note
		}
		if opt.rules {
			note += " " + ruleHelp
		}
	} else {
		note = fmt.Sprintf("This can't be allowed from Slack because %s. Deny it here, or answer in the terminal, which shows it in full.", slackEscaper.Replace(unsafe))
	}
	buttons = append(buttons,
		approvalButton(id, decisionDeny, "Deny", "danger"),
		approvalButton(id, decisionTerminal, "Answer in terminal", ""))
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("", buttons...),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("%s The terminal can answer it too; after %s with no answer here, it is left to the terminal.", note, wait), false, false)),
	}
}

// ruleConfirmBlocks redraws the request with confirm buttons for rule, the
// owner's typed rule: allow and add it for this session, this project or
// all projects. Each button carries the rule's hash, so a later proposal
// can't be granted by an earlier button.
func ruleConfirmBlocks(text, id string, rule allowRule, wait time.Duration) []slack.Block {
	v := id + "|" + rule.hash()
	r := rule.String()
	prefix := "Allow + add"
	warn := ""
	if rule.broad() {
		prefix = "Yes, allow + add BROAD"
		warn = " *This is broad:* it allows every use of " + slackEscaper.Replace(rule.Tool) + "."
	}
	buttons := []slack.BlockElement{
		approvalButton(v, decisionRuleSession, prefix+" "+r+" this session", "primary"),
		approvalButton(v, decisionRuleLocal, prefix+" "+r+" this project", ""),
		approvalButton(v, decisionRuleUser, prefix+" "+r+" all projects", ""),
		approvalButton(id, decisionDeny, "Deny", "danger"),
		approvalButton(id, decisionTerminal, "Answer in terminal", ""),
	}
	note := fmt.Sprintf("Confirm the rule `%s`: click where it applies.%s Or reply `allow <rule>` again to change it, or _no_ to deny. After %s with no answer here, it is left to the terminal.",
		slackEscaper.Replace(r), warn, wait)
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("", buttons...),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, note, false, false)),
	}
}

// approvalOutcome is the line that replaces the buttons once settled.
func approvalOutcome(decision, reason string, wait time.Duration) string {
	switch decision {
	case decisionAllow:
		return "✅ Allowed."
	case decisionAllowSimilar:
		return "✅ Allowed, and added the suggested rule."
	case decisionRuleSession, decisionRuleLocal, decisionRuleUser:
		return fmt.Sprintf("✅ Allowed, and added `%s` (%s).", slackEscaper.Replace(reason), destinationLabel(ruleDestinations[decision]))
	case decisionDeny:
		if reason != "" {
			return "❌ Denied: " + slackEscaper.Replace(reason)
		}
		return "❌ Denied."
	case decisionTerminal:
		return "↩️ Answer it in the terminal."
	}
	return fmt.Sprintf("⏱ No answer after %s; answer it in the terminal.", wait)
}

// endedOutcome replaces the buttons when the host stopped the hook first:
// the terminal answered (or the session moved on), so nothing is decided here.
const endedOutcome = "↩️ No longer waiting: it was answered in the terminal, or the request ended. Nothing was decided here."

// blockedNotice is what the project channel sees when an approval request
// goes unanswered: the agent is now stuck at the terminal, and its peers
// need to know (a silently blocked GM otherwise stalls the whole cohort). It
// names the tool but never its input.
func blockedNotice(agent, tool string, wait time.Duration) string {
	if tool == "" {
		tool = "a tool"
	}
	return fmt.Sprintf("BLOCKED: %s has waited %s for approval of %s; it is now waiting at the terminal.",
		slackEscaper.Replace(agent), wait, slackEscaper.Replace(tool))
}

// permissionDecision is the hook output answering the host's prompt.
func permissionDecision(decision, reason string) map[string]any {
	d := map[string]any{"behavior": decision}
	if decision == decisionDeny {
		msg := "The user denied this in Slack."
		if reason != "" {
			msg = "The user denied this in Slack: " + reason
		}
		d["message"] = msg
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": d}}
}

func newApprovalID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// approvalHook asks the owner in Slack and answers the permission prompt.
// It prints nothing, leaving the prompt to the terminal, when the session
// watches no project, anything fails, the owner picks the terminal, or the
// wait runs out.
func (c *cli) approvalHook(ctx context.Context, ev hookEvent, wait time.Duration) int {
	setup, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	targets, me, err := c.directChannels(setup, c.hookSession(ev))
	if err != nil {
		return 0
	}
	text, unsafe := FormatApproval(me.ownerID, me.agentName, ev.ToolName, ev.ToolInput)
	var opt approvalOptions
	if unsafe == "" && !ev.codex() {
		// Claude Code takes rules back from the hook; Codex rejects them.
		opt.similar, opt.similarLabel = allowSuggestions(ev.PermissionSuggestions)
		opt.rules = true
	}
	id := newApprovalID()
	posted := c.postRequest(setup, targets, "approval request", id, text,
		slack.MsgOptionText(fmt.Sprintf("%s needs your approval: %s", me.agentName, ev.ToolName), false),
		slack.MsgOptionBlocks(approvalBlocks(text, unsafe, id, wait, opt)...))
	if len(posted) == 0 {
		return 0
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, wait)
	var confirm func(allowRule) []slack.Block
	if opt.rules {
		confirm = func(rule allowRule) []slack.Block { return ruleConfirmBlocks(text, id, rule, wait) }
	}
	decision, reason := c.waitForApproval(waitCtx, posted, id, text, approvalHint, confirm)
	cancelWait()
	decision = settleApproval(decision, &reason, unsafe, opt)

	// ctx ends early when the host stops the hook (the terminal answered):
	// finish the redraw regardless.
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), relayTimeout)
	defer cancelFinish()
	outcome := approvalOutcome(decision, reason, wait)
	if decision == decisionEnded || (decision == "" && ctx.Err() != nil) {
		outcome = endedOutcome
	}
	c.finishRequest(finish, posted, id, text, outcome)
	if decision == "" && ctx.Err() == nil {
		c.postBlocked(finish, c.hookSession(ev), me.agentName, ev.ToolName, wait)
	}
	switch decision {
	case decisionAllow, decisionDeny:
		c.printJSON(permissionDecision(decision, reason))
	case decisionAllowSimilar:
		c.printJSON(permissionAllowWith(opt.similar))
	case decisionRuleSession, decisionRuleLocal, decisionRuleUser:
		rule, _ := parseAllowRule(reason)
		c.printJSON(permissionAllowWith([]any{addRule(rule, ruleDestinations[decision])}))
	}
	return 0
}

// settleApproval guards the owner's answer: nothing allows what Slack did
// not show (unsafe), Allow similar needs a suggestion, and a rule answer
// needs a request that takes rules and a rule that still parses. A guarded
// answer goes back to the terminal.
func settleApproval(decision string, reason *string, unsafe string, opt approvalOptions) string {
	switch decision {
	case decisionAllow:
		if unsafe != "" {
			return decisionTerminal // there was no Allow button; never allow what was not shown
		}
	case decisionAllowSimilar:
		if unsafe != "" || len(opt.similar) == 0 {
			return decisionTerminal
		}
	case decisionRuleSession, decisionRuleLocal, decisionRuleUser:
		rule, ok := parseAllowRule(*reason)
		if unsafe != "" || !opt.rules || !ok {
			return decisionTerminal
		}
		*reason = rule.String()
	}
	return decision
}

// permissionAllowWith allows the request and adds permission rules
// (updatedPermissions entries, Claude Code only).
func permissionAllowWith(updates any) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest",
		"decision": map[string]any{"behavior": decisionAllow, "updatedPermissions": updates}}}
}

// postBlocked tells the session's project channels that the agent is stuck
// waiting at the terminal, after the Slack wait ran out. Only projects whose
// cohort the session registered in hear it; others keep the plain behavior.
func (c *cli) postBlocked(ctx context.Context, session, agent, tool string, wait time.Duration) {
	ids, names, err := c.sessionChannels(ctx, session)
	if err != nil {
		return
	}
	for _, id := range ids {
		if _, derived := ProjectOf(names[id]); derived || !c.registeredIn(ctx, session, id) {
			continue // only cohort sessions announce that they are blocked
		}
		if _, _, err := c.bot.PostMessageContext(ctx, id, slack.MsgOptionText(blockedNotice(agent, tool, wait), false)); err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: posting blocked notice to #%s: %v\n", names[id], err)
		}
	}
}

// postRequest posts request id (text is its body) in each target and
// registers every copy with the listener, returning the copies posted.
func (c *cli) postRequest(ctx context.Context, targets []namedChannel, what, id, text string, opts ...slack.MsgOption) []postedMsg {
	return postCopies(targets,
		func(t namedChannel) (string, error) {
			_, ts, err := c.bot.PostMessageContext(ctx, t.id, opts...)
			if err != nil {
				fmt.Fprintf(c.stderr, "slack-agent-chat: posting %s to #%s: %v\n", what, t.name, err)
			}
			return ts, err
		},
		func(p postedMsg) (int, error) {
			resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "approval-watch", Approval: id, Channel: p.channel, TS: p.ts, Text: text})
			if err != nil {
				fmt.Fprintf(c.stderr, "slack-agent-chat: registering %s with the listener: %v\n", what, err)
			}
			return resp.Copies, err
		})
}

// postCopies posts a request in each target and registers each copy (watch
// returns how many copies the listener holds). A further copy is posted only
// after a registration has confirmed a count: a listener that answers
// without one predates multi-copy requests and keeps only the last copy
// registered, so an answer on any other copy would be lost. Until a count
// confirms support (the first registration failed, or the listener is
// older), the request stays at the one copy posted, which the poll
// registers again.
func postCopies(targets []namedChannel, post func(namedChannel) (string, error), watch func(postedMsg) (int, error)) []postedMsg {
	var posted []postedMsg
	for _, t := range targets {
		ts, err := post(t)
		if err != nil {
			continue
		}
		p := postedMsg{t.id, t.name, ts}
		posted = append(posted, p)
		if n, err := watch(p); err != nil || n == 0 {
			return posted // multi-copy support not confirmed: one copy only
		}
	}
	return posted
}

// approvalHint and deniedHint answer an owner who typed an allow word.
const (
	approvalHint = "Replies can only deny (_no_ plus a reason) or send this to the _terminal_; click *Allow* to approve."
	deniedHint   = "Replies can only decline (_no_ plus a reason); click *Approve retry* to approve."
)

// hintTimeout bounds the hint post, so a slow Slack call never stalls the
// polling the listener reads as the hook being alive.
const hintTimeout = 5 * time.Second

// waitForApproval polls the listener for the owner's answer until one
// arrives, ctx ends ("" decision) or the listener ended the request
// (decisionEnded). When the owner types an allow word, it
// posts hint in that copy's thread and keeps waiting. A listener that lost
// the request (it restarted) gets its copies (text) registered again.
// confirm, when set, gives the blocks that redraw the request for a rule the
// owner typed (nil: this request takes no rules).
func (c *cli) waitForApproval(ctx context.Context, posted []postedMsg, id, text, hint string, confirm func(allowRule) []slack.Block) (string, string) {
	tick := time.NewTicker(approvalPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ""
		case <-tick.C:
		}
		resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "approval", Approval: id})
		if err == nil && !resp.Ended && (resp.Unknown || (resp.Copies > 0 && resp.Copies < len(posted))) {
			// The listener restarted, or registering a copy failed: register
			// every copy again (one it has already adds nothing). A listener
			// too old to count copies reports none and is left alone.
			for _, p := range posted {
				_, _ = SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "approval-watch", Approval: id, Channel: p.channel, TS: p.ts, Text: text})
			}
		}
		switch {
		case err != nil:
		case resp.Ended:
			return decisionEnded, ""
		case resp.Decision == "":
		case resp.Decision == decisionHint:
			c.replyInRequest(ctx, posted, resp, hint)
		case resp.Decision == decisionPropose || resp.Decision == decisionRuleHelp:
			rule, ok := parseAllowRule(resp.Text)
			var blocks []slack.Block
			if confirm != nil && ok && resp.Decision == decisionPropose {
				blocks = confirm(rule)
			}
			switch {
			case blocks != nil:
				// Redraw every copy with confirm buttons for this rule.
				for _, p := range posted {
					upd, cancel := context.WithTimeout(ctx, hintTimeout)
					if _, _, _, err := c.bot.UpdateMessageContext(upd, p.channel, p.ts, slack.MsgOptionText("Confirm rule: "+rule.String(), false), slack.MsgOptionBlocks(blocks...)); err != nil {
						fmt.Fprintf(c.stderr, "slack-agent-chat: drawing rule confirm in #%s: %v\n", p.name, err)
					}
					cancel()
				}
			case confirm == nil:
				c.replyInRequest(ctx, posted, resp, "Rules can't be added from Slack for this request: use the approve button to allow just this once, or reply _no_ to deny.")
			default:
				c.replyInRequest(ctx, posted, resp, "I couldn't read that as a rule. "+ruleHelp)
			}
		default:
			return resp.Decision, resp.Text
		}
	}
}

// replyInRequest posts text in the thread of the copy the owner answered in
// (resp.Channel/TS), or the first copy.
func (c *cli) replyInRequest(ctx context.Context, posted []postedMsg, resp ControlResponse, text string) {
	channel, ts := posted[0].channel, posted[0].ts
	for _, p := range posted {
		if p.channel == resp.Channel && p.ts == resp.TS {
			channel, ts = p.channel, p.ts
		}
	}
	post, cancel := context.WithTimeout(ctx, hintTimeout)
	defer cancel()
	if _, _, err := c.bot.PostMessageContext(post, channel, slack.MsgOptionTS(ts), slack.MsgOptionText(text, false)); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: replying to an approval request: %v\n", err)
	}
}

// finishRequest redraws every copy of a settled request with its outcome and
// tells the listener the hook finished. If any redraw failed, the listener
// is given the outcome and redraws the copies again until they all show it,
// so stale buttons never outlive the hook.
func (c *cli) finishRequest(ctx context.Context, posted []postedMsg, id, text, outcome string) {
	owed := ""
	for _, p := range posted {
		if _, _, _, err := c.bot.UpdateMessageContext(ctx, p.channel, p.ts,
			slack.MsgOptionText(outcome, false),
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
				slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, outcome, false, false)),
			)); err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: updating request in #%s: %v\n", p.name, err)
			owed = outcome
		}
	}
	// A budget of its own: the redraw may have used up ctx's.
	end, cancel := context.WithTimeout(context.WithoutCancel(ctx), hintTimeout)
	defer cancel()
	_, _ = SendControl(end, c.home.ControlSocket, ControlRequest{Op: "approval-end", Approval: id, Text: owed})
}
