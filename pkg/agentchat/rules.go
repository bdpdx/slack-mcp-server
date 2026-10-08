package agentchat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Allow rules from Slack (Claude Code only). Claude Code's PermissionRequest
// hook receives the rules its terminal dialog would offer to save
// (permission_suggestions) and may answer "allow" with updatedPermissions,
// the rules to add. Slack offers that two ways:
//   - an Allow similar button, applying Claude Code's own suggestion;
//   - a rule the owner types in a reply, "allow <rule>", which the hook
//     redraws with confirm buttons (this session, this project, all
//     projects). Only a click grants it: anything holding the owner's
//     token, agents included, can post a reply as the owner.
//
// Codex rejects updatedPermissions from hooks, so Codex requests offer
// neither.

// allowRule is a permission rule: Tool, or Tool(Content).
type allowRule struct {
	Tool, Content string
}

func (r allowRule) String() string {
	if r.Content == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Content + ")"
}

// broad reports whether the rule allows a whole tool, or every use of it.
func (r allowRule) broad() bool {
	c := strings.TrimSpace(r.Content)
	return c == "" || c == "*" || c == ":*" || c == "**" || c == "*:*"
}

// hash identifies the rule a confirm button was drawn for, so a click can
// never grant a rule proposed after the button was shown.
func (r allowRule) hash() string {
	sum := sha256.Sum256([]byte(r.String()))
	return hex.EncodeToString(sum[:6])
}

var (
	ruleToolPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)(?:\((.*)\))?$`)
	commandPrefix   = regexp.MustCompile(`^[A-Za-z0-9_./~-][^()\n]*$`)
	// knownTools are tool names a bare word may name (a whole-tool rule).
	knownTools = map[string]bool{"Bash": true, "Read": true, "Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
		"WebFetch": true, "WebSearch": true, "Glob": true, "Grep": true, "Task": true, "Agent": true, "Skill": true}
	// vagueWords open phrases, not command prefixes ("allow all git push").
	vagueWords = map[string]bool{"all": true, "any": true, "every": true, "everything": true, "anything": true,
		"the": true, "this": true, "these": true, "that": true, "those": true, "similar": true, "it": true, "them": true}
)

// ruleHelp shows the owner how to type a rule.
const ruleHelp = "To allow with a rule, reply `allow <rule>`, for example `allow Bash(git push *)` or `allow WebFetch(domain:example.com)`, " +
	"or `allow <command prefix>` for a shell command, for example `allow git push` (which means `Bash(git push *)`). " +
	"You then click a button to choose where the rule applies."

// parseAllowRule reads the rule in an "allow <rule>" reply (text is what
// follows "allow"). ok is false when it is not a rule this accepts.
func parseAllowRule(text string) (allowRule, bool) {
	s := strings.TrimSpace(text)
	s = strings.Trim(s, "`'\"")
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 300 || strings.ContainsAny(s, "\n\r") {
		return allowRule{}, false
	}
	if m := ruleToolPattern.FindStringSubmatch(s); m != nil {
		tool := m[1]
		if strings.Contains(s, "(") {
			content := strings.TrimSpace(m[2])
			if content == "" || strings.ContainsAny(content, "()") {
				return allowRule{}, false
			}
			if !knownTools[tool] && !strings.HasPrefix(tool, "mcp__") {
				return allowRule{}, false
			}
			return allowRule{Tool: tool, Content: content}, true
		}
		if knownTools[tool] || strings.HasPrefix(tool, "mcp__") {
			return allowRule{Tool: tool}, true
		}
	}
	// A shell command prefix: "git push" means Bash(git push *).
	if !commandPrefix.MatchString(s) {
		return allowRule{}, false
	}
	first := strings.ToLower(strings.Fields(s)[0])
	if vagueWords[first] {
		return allowRule{}, false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "*"))
	if prefix == "" {
		return allowRule{}, false
	}
	return allowRule{Tool: "Bash", Content: prefix + " *"}, true
}

// ruleProposal splits an owner's reply into "allow" and what follows it:
// isRule when the reply opens with "allow" followed by more text.
func ruleProposal(text string) (rest string, isRule bool) {
	text = strings.TrimSpace(leadMentions.ReplaceAllString(text, ""))
	m := firstWord.FindStringSubmatch(text)
	if m == nil || strings.ToLower(strings.Trim(m[1], ".,!:;-—*_~`")) != "allow" {
		return "", false
	}
	rest = strings.TrimSpace(text[len(m[0]):])
	return rest, rest != ""
}

// suggestion is one entry of Claude Code's permission_suggestions.
type suggestion struct {
	Type        string `json:"type"`
	Behavior    string `json:"behavior"`
	Destination string `json:"destination"`
	Rules       []struct {
		ToolName    string `json:"toolName"`
		RuleContent string `json:"ruleContent,omitempty"`
	} `json:"rules"`
}

// allowSuggestions keeps the suggestions that add allow rules, as raw JSON
// to hand back unchanged, with a label naming their rules and destination.
func allowSuggestions(raw json.RawMessage) (keep []json.RawMessage, label string) {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil, ""
	}
	var parts []string
	for _, e := range entries {
		var s suggestion
		if json.Unmarshal(e, &s) != nil || s.Type != "addRules" || s.Behavior != "allow" || len(s.Rules) == 0 {
			continue
		}
		var rules []string
		for _, r := range s.Rules {
			if r.ToolName == "" {
				continue
			}
			rules = append(rules, "`"+allowRule{Tool: r.ToolName, Content: r.RuleContent}.String()+"`")
		}
		if len(rules) == 0 {
			continue
		}
		keep = append(keep, e)
		parts = append(parts, fmt.Sprintf("%s (%s)", strings.Join(rules, ", "), destinationLabel(s.Destination)))
	}
	return keep, strings.Join(parts, "; ")
}

// destinationLabel names where a rule is saved.
func destinationLabel(d string) string {
	switch d {
	case "session":
		return "this session"
	case "localSettings":
		return "this project"
	case "projectSettings":
		return "this project, shared"
	case "userSettings":
		return "all projects"
	}
	return d
}

// ruleDestinations maps the rule confirm decisions to Claude Code
// destinations.
var ruleDestinations = map[string]string{
	decisionRuleSession: "session",
	decisionRuleLocal:   "localSettings",
	decisionRuleUser:    "userSettings",
}

// addRule is the updatedPermissions entry adding rule at destination.
func addRule(rule allowRule, destination string) map[string]any {
	r := map[string]any{"toolName": rule.Tool}
	if rule.Content != "" {
		r["ruleContent"] = rule.Content
	}
	return map[string]any{"type": "addRules", "rules": []any{r}, "behavior": "allow", "destination": destination}
}
