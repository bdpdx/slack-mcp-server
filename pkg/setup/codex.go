package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	autoReviewLine = regexp.MustCompile(`(?m)^\s*approvals_reviewer\s*=\s*"auto_review"\s*(#.*)?$`)
	notifyAssign   = regexp.MustCompile(`(?m)^(\s*notify\s*=\s*)(\[.*\])\s*$`)
)

// usesAutoReview reports whether a Codex config.toml routes approvals
// through auto_review (which runs after PermissionRequest hooks).
func usesAutoReview(cfg string) bool { return autoReviewLine.MatchString(cfg) }

// fixNotify drops a `--previous-notify <command>` pair from notify when the
// command is codex-push.py, which Slack turn-end DMs replace.
func fixNotify(cfg string) (string, bool, error) {
	m := notifyAssign.FindStringSubmatchIndex(cfg)
	if m == nil {
		return cfg, false, nil
	}
	var arr []string
	if json.Unmarshal([]byte(cfg[m[4]:m[5]]), &arr) != nil {
		return cfg, false, nil // not a simple array; leave it alone
	}
	var out []string
	changed := false
	for i := 0; i < len(arr); i++ {
		if arr[i] == "--previous-notify" && i+1 < len(arr) && strings.Contains(arr[i+1], "codex-push.py") {
			i++
			changed = true
			continue
		}
		out = append(out, arr[i])
	}
	if !changed {
		return cfg, false, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return cfg, false, err
	}
	val := strings.ReplaceAll(strings.TrimSpace(buf.String()), `","`, `", "`)
	return cfg[:m[4]] + val + cfg[m[5]:], true, nil
}

// approveSlackTools sets default_tools_approval_mode = "approve" (Codex's
// "run tools automatically") in config.toml's [mcp_servers.slack] table, so
// Slack MCP calls don't each need approval. A value the user set is kept;
// without a slack table nothing changes. Only that table is touched.
func approveSlackTools(cfg string) (string, bool) {
	lines := strings.SplitAfter(cfg, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "[mcp_servers.slack]" {
			start = i
			break
		}
	}
	if start < 0 {
		return cfg, false
	}
	last := start // the table's last key line
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") {
			break
		}
		if strings.HasPrefix(t, "default_tools_approval_mode") {
			return cfg, false
		}
		if t != "" && !strings.HasPrefix(t, "#") {
			last = i
		}
	}
	if !strings.HasSuffix(lines[last], "\n") {
		lines[last] += "\n"
	}
	insert := "default_tools_approval_mode = \"approve\"\n"
	out := strings.Join(lines[:last+1], "") + insert + strings.Join(lines[last+1:], "")
	return out, true
}

// codexRule lets Codex run `<bin> chat …` without asking.
func codexRule(bin string) string {
	q, _ := json.Marshal(bin)
	return `prefix_rule(pattern=[` + string(q) + `, "chat"], decision="allow")`
}

var chatRuleLine = regexp.MustCompile(`^\s*prefix_rule\(\s*pattern\s*=\s*\[\s*("(?:[^"\\]|\\.)*")\s*,\s*"chat"\s*[,\]]`)

// isOurChatRule reports whether line is a slack-mcp-server "chat"
// prefix_rule: base name slack-mcp-server, or resolving to binReal.
func isOurChatRule(line, binReal string, binErr error) bool {
	m := chatRuleLine.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	var path string
	if json.Unmarshal([]byte(m[1]), &path) != nil {
		return false
	}
	if filepath.Base(path) == "slack-mcp-server" {
		return true
	}
	real, err := filepath.EvalSymlinks(path)
	return err == nil && binErr == nil && real == binReal
}

// withoutCodexRules drops every slack-mcp-server "chat" prefix_rule and keeps
// all other lines as they are.
func withoutCodexRules(text, bin string) string {
	binReal, binErr := filepath.EvalSymlinks(bin)
	var out []string
	for _, line := range strings.SplitAfter(text, "\n") {
		if line != "" && !isOurChatRule(line, binReal, binErr) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "")
}

// withCodexRule drops every slack-mcp-server "chat" prefix_rule (any path)
// except one equal to rule, keeps all other lines as they are, and appends
// rule if it is not already present.
func withCodexRule(text, rule, bin string) string {
	binReal, binErr := filepath.EvalSymlinks(bin)
	var out []string
	have := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if strings.TrimSpace(line) == rule {
			if have {
				continue
			}
			have = true
		} else if isOurChatRule(line, binReal, binErr) {
			continue
		}
		out = append(out, line)
	}
	res := strings.Join(out, "")
	if !have {
		if res != "" && !strings.HasSuffix(res, "\n") {
			res += "\n"
		}
		res += rule + "\n"
	}
	return res
}

// InstallCodex installs the skill, hooks, rule, notify cleanup (asked) and
// MCP registration, plus the app-server launch agent, in a Codex home. The env file is written separately.
func InstallCodex(home, userHome, bin string, r Runner, p Prompter, now time.Time) (Result, error) {
	res := Result{Home: home}
	cfgPath := filepath.Join(home, "config.toml")
	cfgData, _ := os.ReadFile(cfgPath)
	cfg := string(cfgData)
	autoReview := usesAutoReview(cfg)

	hooksPath := filepath.Join(home, "hooks.json")
	doc, err := readJSONObject(hooksPath)
	if err != nil {
		return res, err
	}
	var events map[string]any
	if raw, ok := doc["hooks"]; ok && raw != nil {
		if events, ok = raw.(map[string]any); !ok {
			return res, fmt.Errorf("%s: \"hooks\" is not a JSON object; fix it and run setup again", hooksPath)
		}
	}
	changed, err := InstallSkill(home, TypeCodex, bin, now)
	res.Changed = append(res.Changed, changed...)
	if err != nil {
		return res, err
	}
	doc["hooks"] = MergeHooks(events, CodexHookSpecs(!autoReview), bin, EnvPath(home), "SlackAgentChat")
	if wrote, err := writeJSONObject(hooksPath, doc, now); err != nil {
		return res, err
	} else if wrote {
		res.Changed = append(res.Changed, hooksPath)
		res.Stale = true // Codex reads (and asks to trust) hooks when a session starts
	}
	if autoReview {
		res.Notes = append(res.Notes, "approval requests stay with Codex's auto_review (no Slack approval hook)")
	}

	rulesPath := filepath.Join(home, "rules", "default.rules")
	rules, _ := os.ReadFile(rulesPath)
	if next := withCodexRule(string(rules), codexRule(bin), bin); next != string(rules) {
		if _, err := replaceFile(rulesPath, []byte(next), 0o600, now); err != nil {
			return res, err
		}
		res.Changed = append(res.Changed, rulesPath)
	}

	fixed, ok, err := fixNotify(cfg)
	if err != nil {
		return res, err
	}
	if ok {
		yes, err := p.Confirm("Codex's notify runs codex-push.py; Slack turn-end DMs replace it. Remove it from notify?", true)
		if err != nil {
			return res, err
		}
		if yes {
			if _, err := replaceFile(cfgPath, []byte(fixed), 0o600, now); err != nil {
				return res, err
			}
			res.Changed = append(res.Changed, cfgPath)
			res.Stale = true
		}
	}

	if _, err := r.LookPath("codex"); err == nil {
		// codex mcp remove/add rewrite config.toml.
		if _, err := backup(cfgPath, now); err != nil {
			return res, err
		}
	}
	manual, err := RegisterMCP(r, TypeCodex, home, "", bin)
	if err != nil {
		return res, err
	}
	if manual != "" {
		res.Manual = append(res.Manual, manual)
	} else {
		res.Changed = append(res.Changed, "MCP server registered with codex")
		// codex mcp add rewrites the slack table, so this comes after it.
		data, err := os.ReadFile(cfgPath)
		if err != nil && !os.IsNotExist(err) {
			return res, err
		}
		if updated, ok := approveSlackTools(string(data)); ok {
			if _, err := replaceFile(cfgPath, []byte(updated), 0o600, now); err != nil {
				return res, err
			}
			res.Changed = append(res.Changed, cfgPath+": Slack MCP tools run without asking (default_tools_approval_mode = \"approve\")")
			res.Stale = true
		}
	}

	asChanged, asNotes, err := ensureAppServer(home, userHome, r, p)
	res.Changed = append(res.Changed, asChanged...)
	res.Notes = append(res.Notes, asNotes...)
	if err != nil {
		return res, err
	}

	script, wrote, err := installStartScript(home, bin, now)
	if err != nil {
		return res, err
	}
	if wrote {
		res.Changed = append(res.Changed, script)
	}
	res.Notes = append(res.Notes, fmt.Sprintf("start Codex for this home with %s, run from your project directory (or pass -p <dir>, or set PROJECT_ROOT in the script); -m daybreak picks gpt-daybreak-blue-latest", filepath.Base(script)))
	return res, nil
}
