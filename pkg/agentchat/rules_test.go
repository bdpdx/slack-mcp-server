package agentchat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllowRule(t *testing.T) {
	for in, want := range map[string]string{
		"Bash(git push *)":        "Bash(git push *)",
		"`Bash(npm run lint)`":    "Bash(npm run lint)",
		"git push":                "Bash(git push *)",
		"git push*":               "Bash(git push *)",
		"./bin/check-deploy":      "Bash(./bin/check-deploy *)",
		"WebFetch":                "WebFetch",
		"WebFetch(domain:x.com)":  "WebFetch(domain:x.com)",
		"mcp__linear__save_issue": "mcp__linear__save_issue",
		"Bash(*)":                 "Bash(*)",
		"  Read(//Users/b/**)  ":  "Read(//Users/b/**)",
	} {
		r, ok := parseAllowRule(in)
		require.True(t, ok, in)
		assert.Equal(t, want, r.String(), in)
	}
	for _, in := range []string{"", "all git push", "every git command", "the similar ones", "Foo(bar)", "Bash()",
		"Bash(echo $(date))", "git push (force)", "line\nbreak", "*"} {
		_, ok := parseAllowRule(in)
		assert.False(t, ok, in)
	}
	broad, _ := parseAllowRule("WebFetch")
	assert.True(t, broad.broad())
	star, _ := parseAllowRule("Bash(*)")
	assert.True(t, star.broad())
	narrow, _ := parseAllowRule("git push")
	assert.False(t, narrow.broad())
}

func TestRuleProposal(t *testing.T) {
	rest, ok := ruleProposal("<@UCL> allow git push")
	assert.True(t, ok)
	assert.Equal(t, "git push", rest)
	_, ok = ruleProposal("allow")
	assert.False(t, ok, "plain allow is the click hint, not a rule")
	_, ok = ruleProposal("yes please")
	assert.False(t, ok)
}

func TestAllowSuggestions(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"git push *"}],"behavior":"allow","destination":"localSettings"},
		{"type":"setMode","mode":"acceptEdits","destination":"session"},
		{"type":"addRules","rules":[{"toolName":"Read"}],"behavior":"deny","destination":"session"}
	]`)
	keep, label := allowSuggestions(raw)
	require.Len(t, keep, 1, "only allow-adding rules")
	assert.Equal(t, "`Bash(git push *)` (this project)", label)
	none, _ := allowSuggestions(nil)
	assert.Empty(t, none)
}

func TestSettleApproval(t *testing.T) {
	opt := approvalOptions{similar: []json.RawMessage{json.RawMessage(`{}`)}, rules: true}
	r := "git push"
	assert.Equal(t, decisionRuleLocal, settleApproval(decisionRuleLocal, &r, "", opt))
	assert.Equal(t, "Bash(git push *)", r, "normalized")
	r = "all git push"
	assert.Equal(t, decisionTerminal, settleApproval(decisionRuleLocal, &r, "", opt), "an unparseable rule never allows")
	r = "git push"
	assert.Equal(t, decisionTerminal, settleApproval(decisionRuleUser, &r, "", approvalOptions{}), "Codex takes no rules")
	assert.Equal(t, decisionTerminal, settleApproval(decisionAllowSimilar, &r, "", approvalOptions{}), "no suggestion, no similar")
	assert.Equal(t, decisionTerminal, settleApproval(decisionAllow, &r, "input withheld", opt), "unsafe never allows")
	assert.Equal(t, decisionTerminal, settleApproval(decisionRuleSession, &r, "input withheld", opt))
}

func TestPermissionAllowWith(t *testing.T) {
	rule, _ := parseAllowRule("git push")
	data, _ := json.Marshal(permissionAllowWith([]any{addRule(rule, "localSettings")}))
	assert.JSONEq(t, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow",
		"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"git push *"}],"behavior":"allow","destination":"localSettings"}]}}}`, string(data))
	whole, _ := parseAllowRule("WebFetch")
	data, _ = json.Marshal(addRule(whole, "session"))
	assert.NotContains(t, string(data), "ruleContent", "a whole-tool rule has no content")
}

func TestHookEventKind(t *testing.T) {
	assert.True(t, hookEvent{TurnID: "t1"}.codex())
	assert.False(t, hookEvent{SessionID: "s", PromptID: "p"}.codex())
}

func TestRuleConfirmBlocks(t *testing.T) {
	rule, _ := parseAllowRule("git push")
	data, _ := json.Marshal(ruleConfirmBlocks("req", "abc", rule, defaultApprovalWait))
	for _, d := range []string{decisionRuleSession, decisionRuleLocal, decisionRuleUser} {
		assert.Contains(t, string(data), `"action_id":"`+approvalActionPrefix+d+`","value":"abc|`+rule.hash()+`"`)
	}
	broad, _ := parseAllowRule("Bash(*)")
	data, _ = json.Marshal(ruleConfirmBlocks("req", "abc", broad, defaultApprovalWait))
	assert.Contains(t, string(data), "BROAD")
}

// A typed rule is proposed, then granted only by the owner's click on a
// confirm button drawn for that exact rule.
func TestListenerRuleFlow(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "r1", Channel: "C1", TS: "2000.1"}).OK)
	reply := func(ts, text string) {
		l.HandleMessage(ctx, Message{Channel: "C1", TS: ts, ThreadTS: "2000.1", User: "UBR", Text: text})
	}

	reply("2000.2", "allow all git push")
	resp := l.Control(ctx, ControlRequest{Op: "approval", Approval: "r1"})
	assert.Equal(t, decisionRuleHelp, resp.Decision, "not a rule: the hook answers with the format")
	assert.Equal(t, "all git push", resp.Text)

	reply("2000.3", "allow git push")
	resp = l.Control(ctx, ControlRequest{Op: "approval", Approval: "r1"})
	require.Equal(t, decisionPropose, resp.Decision)
	assert.Equal(t, "Bash(git push *)", resp.Text)
	assert.Equal(t, "C1", resp.Channel)
	rule, _ := parseAllowRule("git push")

	old, _ := parseAllowRule("git status")
	l.HandleInteraction(clickLabeled("UBR", decisionRuleLocal, "r1|"+old.hash(), "C1", "2000.1", ruleButtonLabel(old, decisionRuleLocal)))
	d, _ := takeApproval(t, l, "r1")
	assert.Equal(t, "", d, "a button drawn for another rule grants nothing")

	l.HandleInteraction(clickLabeled("UMI", decisionRuleLocal, "r1|"+rule.hash(), "C1", "2000.1", ruleButtonLabel(rule, decisionRuleLocal)))
	d, _ = takeApproval(t, l, "r1")
	assert.Equal(t, "", d, "only the owner")

	l.HandleInteraction(clickLabeled("UBR", decisionRuleLocal, "r1|"+rule.hash(), "C1", "2000.1", "Allow"))
	d, _ = takeApproval(t, l, "r1")
	assert.Equal(t, "", d, "a rule button relabelled to look like something else grants nothing")

	l.HandleInteraction(clickLabeled("UBR", decisionRuleLocal, "r1|"+rule.hash(), "C1", "2000.1", ruleButtonLabel(rule, decisionRuleLocal)))
	d, reason := takeApproval(t, l, "r1")
	assert.Equal(t, decisionRuleLocal, d)
	assert.Equal(t, "Bash(git push *)", reason)
}

// Allow similar is a click like Allow: it decides only on a registered copy.
func TestListenerAllowSimilarClick(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "s1", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickLabeled("UBR", decisionAllowSimilar, "s1", "C1", "2000.1", "Allow + add Bash(*) all projects"))
	d, _ := takeApproval(t, l, "s1")
	assert.Equal(t, "", d, "relabelled: grants only what its label said")
	l.HandleInteraction(clickLabeled("UBR", decisionAllowSimilar, "s1", "C1", "2000.1", similarLabel))
	d, _ = takeApproval(t, l, "s1")
	assert.Equal(t, decisionAllowSimilar, d)
}

// clickLabeled is a click on a button whose text is label.
func clickLabeled(user, decision, value, channel, ts, label string) []byte {
	return []byte(`{"type":"block_actions","user":{"id":"` + user + `"},` +
		`"container":{"type":"message","channel_id":"` + channel + `","message_ts":"` + ts + `"},` +
		`"message":{"bot_id":"BCL"},` +
		`"actions":[{"action_id":"` + approvalActionPrefix + decision + `","value":"` + value + `","text":{"type":"plain_text","text":"` + label + `"}}]}`)
}

// Slack escapes & < > and wraps links in posted text; rules are read as typed.
func TestRuleProposalUndoesSlackEscaping(t *testing.T) {
	rest, _ := ruleProposal("allow Bash(cat a &amp;&amp; b)")
	r, ok := parseAllowRule(rest)
	require.True(t, ok)
	assert.Equal(t, "Bash(cat a && b)", r.String())
	rest, _ = ruleProposal("allow WebFetch(domain:<http://example.com|example.com>)")
	r, ok = parseAllowRule(rest)
	require.True(t, ok)
	assert.Equal(t, "WebFetch(domain:example.com)", r.String())
	r, _ = parseAllowRule("git push **")
	assert.Equal(t, "Bash(git push *)", r.String())
}

// A rule click waiting for its copy is applied only if it is still for the
// rule proposed when the copy registers.
func TestPendingRuleClickChecksTheProposal(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "p1", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.2", ThreadTS: "2000.1", User: "UBR", Text: "allow git push"})
	takeApproval(t, l, "p1")
	rule, _ := parseAllowRule("git push")
	// clicked on a copy that isn't registered yet
	l.HandleInteraction(clickLabeled("UBR", decisionRuleUser, "p1|"+rule.hash(), "C2", "3000.1", ruleButtonLabel(rule, decisionRuleUser)))
	// the owner then proposes a different rule
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.3", ThreadTS: "2000.1", User: "UBR", Text: "allow git status"})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "p1", Channel: "C2", TS: "3000.1"}).OK)
	d, _ := takeApproval(t, l, "p1")
	assert.Equal(t, decisionPropose, d, "the earlier click is dropped; the new proposal is handed on")
}

// The confirm view keeps a one-time Allow as its default choice.
func TestConfirmKeepsAllowOnce(t *testing.T) {
	broad, _ := parseAllowRule("Bash(*)")
	data, _ := json.Marshal(ruleConfirmBlocks("req", "abc", broad, defaultApprovalWait))
	s := string(data)
	assert.Contains(t, s, `"action_id":"`+approvalActionPrefix+decisionAllow+`","value":"abc"`)
	assert.Contains(t, s, "Allow once")
	assert.Equal(t, 1, strings.Count(s, `"style":"primary"`), "only Allow once is primary")
}

// A rule proposed but not yet handed to the hook holds the shutdown drain.
func TestProposalHoldsDrain(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "q1", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.2", ThreadTS: "2000.1", User: "UBR", Text: "allow git push"})
	l.mu.Lock()
	waiting := l.answersWaitingLocked()
	l.mu.Unlock()
	assert.True(t, waiting)
}

func TestRuleHelpListsFormats(t *testing.T) {
	assert.True(t, strings.Contains(ruleHelp, "allow Bash(git push *)") && strings.Contains(ruleHelp, "allow git push"))
}

// Every fixed button grants only what its label said: a Deny relabelled as
// Allow, or an Allow relabelled as Deny, is dropped.
func TestFixedButtonsCheckTheirLabels(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "f1", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickLabeled("UBR", decisionAllow, "f1", "C1", "2000.1", "Deny"))
	l.HandleInteraction(clickLabeled("UBR", decisionDeny, "f1", "C1", "2000.1", "Allow"))
	d, _ := takeApproval(t, l, "f1")
	assert.Equal(t, "", d, "relabelled buttons decide nothing")
	l.HandleInteraction(clickLabeled("UBR", decisionAllow, "f1", "C1", "2000.1", "Approve retry"))
	d, _ = takeApproval(t, l, "f1")
	assert.Equal(t, decisionAllow, d, "the denied-hook's label counts")
}
