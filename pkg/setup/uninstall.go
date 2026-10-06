package setup

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bdpdx/slack-mcp-server/skills"
)

// unHome is a home to offer for uninstall.
type unHome struct {
	Home
	missing bool // the folder is gone; only outside-home pieces remain
}

// notLoadedRe matches launchctl's complaints about an agent that is not
// loaded, which uninstall treats as already done.
var notLoadedRe = regexp.MustCompile(`(?i)not loaded|no such process|could not find|not found`)

// looksInstalled reports whether a standard home holds our env file or hooks.
func looksInstalled(path string) bool {
	if exists(EnvPath(path)) {
		return true
	}
	for _, f := range []string{"settings.json", "hooks.json"} {
		doc, err := readJSONObject(filepath.Join(path, f))
		if err != nil {
			continue
		}
		if events, ok := doc["hooks"].(map[string]any); ok {
			if _, n := removeOurHooks(events); n > 0 {
				return true
			}
		}
	}
	return false
}

// uninstallHomes lists every saved home (even with its folder gone), plus
// ~/.claude and ~/.codex when they look installed.
func uninstallHomes(userHome string, st *State) []unHome {
	var homes []unHome
	have := map[string]bool{}
	add := func(path, typ string) {
		if have[path] {
			return
		}
		have[path] = true
		homes = append(homes, unHome{Home: Home{Path: path, Type: typ, HasEnv: exists(EnvPath(path))}, missing: !exists(path)})
	}
	for _, h := range st.Homes {
		add(h.Path, h.Type)
	}
	for _, std := range []struct{ dir, typ string }{{".claude", TypeClaude}, {".codex", TypeCodex}} {
		if p := filepath.Join(userHome, std.dir); exists(p) && looksInstalled(p) {
			add(p, std.typ)
		}
	}
	return homes
}

// uninstaller holds what the per-home steps share.
type uninstaller struct {
	o   Options
	bin string // the linked binary, which locates the start scripts
	res *Result
	// failed is set when a step failed, so the home stays in the state file.
	failed bool
}

func (u *uninstaller) fail(step string, err error) {
	u.failed = true
	u.res.Notes = append(u.res.Notes, fmt.Sprintf("FAILED: %s: %s. Fix this, then run ./uninstall.sh again.", step, strings.TrimRight(err.Error(), ".")))
}

func (u *uninstaller) removed(what string) { u.res.Changed = append(u.res.Changed, "removed "+what) }
func (u *uninstaller) note(f string, a ...any) {
	u.res.Notes = append(u.res.Notes, fmt.Sprintf(f, a...))
}

// removeFile backs up path next to it and deletes it.
func (u *uninstaller) removeFile(path string) error {
	if _, err := backup(path, u.o.Now()); err != nil {
		return err
	}
	return os.Remove(path)
}

// removeAway backs up path into <repo>/.install/backups (private, so a plist
// is not loaded by launchd and a script is not on PATH), deletes it and
// reports where the backup went.
func (u *uninstaller) removeAway(path string) error {
	dst, err := backupInto(path, filepath.Join(u.o.Repo, ".install", "backups"), 0o600, u.o.Now())
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	u.removed(path)
	u.note("backup of %s: %s", filepath.Base(path), dst)
	return nil
}

func (u *uninstaller) hooks(h Home) {
	name := "settings.json"
	if h.Type == TypeCodex {
		name = "hooks.json"
	}
	path := filepath.Join(h.Path, name)
	doc, err := readJSONObject(path)
	if err != nil {
		u.fail("hooks", err)
		return
	}
	raw, present := doc["hooks"]
	if !present || raw == nil {
		return
	}
	events, ok := raw.(map[string]any)
	if !ok {
		u.fail("hooks", fmt.Errorf("%s: \"hooks\" is not a JSON object", path))
		return
	}
	kept, n := removeOurHooks(events)
	if n == 0 {
		return
	}
	if len(kept) == 0 {
		delete(doc, "hooks")
	} else {
		doc["hooks"] = kept
	}
	if _, err := writeJSONObject(path, doc, u.o.Now()); err != nil {
		u.fail("hooks", err)
		return
	}
	u.removed(fmt.Sprintf("%d slack-mcp-server hook(s) from %s", n, path))
}

func (u *uninstaller) skill(h Home) {
	files, err := skills.Files(h.Type)
	if err != nil {
		u.fail("skill", err)
		return
	}
	dir := filepath.Join(h.Path, "skills", "slack-agent-chat")
	for n := range files {
		p := filepath.Join(dir, n)
		if !exists(p) {
			continue
		}
		if err := u.removeFile(p); err != nil {
			u.fail("skill", err)
			return
		}
		u.removed(p)
	}
	if err := os.Remove(dir); err != nil && exists(dir) { // only when empty
		u.note("%s kept: it still holds the skill file backups (and any files you added)", dir)
	}
}

func (u *uninstaller) mcp(h unHome) {
	name, rm := mcpRemoveArgs(h.Type)
	env := mcpEnv(h.Type, h.Path, u.o.UserHome)
	defaultClaude := h.Type == TypeClaude && env[0] == "CLAUDE_CONFIG_DIR"
	if h.missing && !defaultClaude {
		u.note("MCP registration: skipped (folder missing); if %s still lists \"slack\", run: %s", name, manualCommand(env, name, rm))
		return
	}
	if _, err := u.o.R.LookPath(name); err != nil {
		u.res.Manual = append(u.res.Manual, manualCommand(env, name, rm))
		return
	}
	if out, err := u.o.R.Run(env, name, rm...); err != nil {
		u.note("%s mcp remove reported: %s (the slack server may not have been registered)", name, strings.TrimSpace(out+" "+err.Error()))
		return
	}
	u.removed("MCP server registration from " + name)
}

func (u *uninstaller) rule(h Home) {
	path := filepath.Join(h.Path, "rules", "default.rules")
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			u.fail("rule", err)
		}
		return
	}
	next := withoutCodexRules(string(data), u.bin)
	if next == string(data) {
		return
	}
	if _, err := replaceFile(path, []byte(next), 0o600, u.o.Now()); err != nil {
		u.fail("rule", err)
		return
	}
	u.removed("the Slack chat rule from " + path)
}

// withoutApproveLine drops default_tools_approval_mode = "approve" from the
// [mcp_servers.slack] table only.
func withoutApproveLine(cfg string) (string, bool) {
	lines := strings.SplitAfter(cfg, "\n")
	in, changed := false, false
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			in = t == "[mcp_servers.slack]"
		}
		if in && strings.HasPrefix(t, "default_tools_approval_mode") && strings.Contains(t, `"approve"`) {
			changed = true
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, ""), changed
}

func (u *uninstaller) config(h Home) {
	path := filepath.Join(h.Path, "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			u.fail("config.toml", err)
		}
		return
	}
	next, ok := withoutApproveLine(string(data))
	if !ok {
		return
	}
	if _, err := replaceFile(path, []byte(next), 0o600, u.o.Now()); err != nil {
		u.fail("config.toml", err)
		return
	}
	u.removed("the approve setting for Slack tools from " + path)
}

// env deletes the env file with no backup: a backup would keep the tokens.
func (u *uninstaller) env(h Home) {
	err := os.Remove(EnvPath(h.Path))
	switch {
	case err == nil:
		u.removed(EnvPath(h.Path))
	case !errors.Is(err, os.ErrNotExist):
		u.fail("env file", err)
	}
}

func (u *uninstaller) launchAgent(h Home) error {
	label := appServerLabel(h.Path)
	plist := filepath.Join(u.o.UserHome, "Library", "LaunchAgents", label+".plist")
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	hasPlist := exists(plist)
	if !hasPlist {
		if _, err := u.o.R.Run(nil, "launchctl", "print", target); err != nil {
			return nil // not loaded and no plist: nothing to do
		}
	}
	yes, err := u.o.P.Confirm(fmt.Sprintf("Uninstall the app-server launch agent %s? Other processes might be using it.", label), true)
	if err != nil || !yes {
		if err == nil {
			u.note("launch agent %s kept", label)
		}
		return err
	}
	if out, err := u.o.R.Run(nil, "launchctl", "bootout", target); err != nil && !notLoadedRe.MatchString(out+err.Error()) {
		u.fail("launch agent", fmt.Errorf("launchctl bootout %s failed: %v: %s", target, err, strings.TrimSpace(out)))
		return nil
	}
	u.removed("app-server launch agent " + label)
	if hasPlist {
		if err := u.removeAway(plist); err != nil {
			u.fail("launch agent plist", err)
		}
	}
	return nil
}

func (u *uninstaller) startScript(h Home) error {
	if u.bin == "" {
		return nil
	}
	path := filepath.Join(filepath.Dir(u.bin), startScriptName(h.Path))
	if !exists(path) {
		return nil
	}
	yes, err := u.o.P.Confirm("Delete "+path+"?", true)
	if err != nil || !yes {
		if err == nil {
			u.note("%s kept", path)
		}
		return err
	}
	if err := u.removeAway(path); err != nil {
		u.fail("start script", err)
	}
	return nil
}

// uninstallHome runs the steps for one home the user said yes to.
func (u *uninstaller) uninstallHome(h unHome) error {
	if h.missing {
		skipped := "hooks, skill and env file"
		if h.Type == TypeCodex {
			skipped = "hooks, skill, rule, config and env file"
		}
		u.note("folder missing: %s skipped", skipped)
	} else {
		u.hooks(h.Home)
		u.skill(h.Home)
	}
	u.mcp(h)
	if !h.missing {
		if h.Type == TypeCodex {
			u.rule(h.Home)
			u.config(h.Home)
		}
		u.env(h.Home)
	}
	if h.Type != TypeCodex {
		return nil
	}
	if err := u.launchAgent(h.Home); err != nil {
		return err
	}
	return u.startScript(h.Home)
}

// Uninstall removes slack-mcp-server's agent chat setup from the homes the
// user confirms, one at a time.
func Uninstall(o Options) ([]Result, error) {
	statePath := filepath.Join(o.Repo, ".install-state.json")
	st, err := LoadState(statePath)
	if err != nil {
		return nil, err
	}
	homes := uninstallHomes(o.UserHome, st)
	bin := st.Bin
	if bin == "" {
		var existing []Home
		for _, h := range homes {
			existing = append(existing, h.Home)
		}
		if bin = ExistingBin(existing); bin == "" {
			bin = o.Bin
		}
	}
	o.P.Say("Agent homes found:")
	for _, h := range homes {
		gone := ""
		if h.missing {
			gone = " (folder missing)"
		}
		o.P.Say("  %s [%s]%s", h.Path, h.Type, gone)
	}
	var results []Result
	kept := 0
	for _, h := range homes {
		res := Result{Home: h.Path, Type: h.Type}
		if hs := st.Home(h.Path); hs != nil {
			res.Bot = hs.Bot
		}
		yes, err := o.P.Confirm(fmt.Sprintf("Uninstall slack-mcp-server from %s?", h.Path), false)
		if err != nil {
			return results, err
		}
		if !yes {
			kept++
			res.Notes = append(res.Notes, "Skipped.")
			results = append(results, res)
			continue
		}
		u := &uninstaller{o: o, bin: bin, res: &res}
		if err := u.uninstallHome(h); err != nil {
			return append(results, res), err
		}
		res.Removed = true
		results = append(results, res)
		if u.failed {
			continue
		}
		if st.Home(h.Path) != nil {
			dropState(st, h.Path)
			if err := st.Save(statePath); err != nil {
				return results, err
			}
		}
	}
	if len(st.Homes) == 0 && kept == 0 {
		removeBinLink(o, bin, &results)
	}
	return results, nil
}

// removeBinLink offers to remove the binary symlink once no home needs it;
// a regular file is never removed.
func removeBinLink(o Options, bin string, results *[]Result) {
	info, err := os.Lstat(bin)
	if bin == "" || err != nil || info.Mode()&os.ModeSymlink == 0 {
		return
	}
	yes, err := o.P.Confirm("Remove the binary link "+bin+"?", true)
	if err != nil || !yes {
		return
	}
	res := Result{Home: "Binary link"}
	if err := os.Remove(bin); err != nil {
		res.Notes = append(res.Notes, "FAILED: "+err.Error())
	} else {
		res.Changed = append(res.Changed, "removed "+bin)
	}
	*results = append(*results, res)
}

func printUninstallSummary(w io.Writer, results []Result) {
	fmt.Fprintln(w, "\nSummary")
	var bots []string
	seen, removed := map[string]bool{}, false
	for _, r := range results {
		fmt.Fprintf(w, "\n%s\n", r.Home)
		for _, c := range r.Changed {
			fmt.Fprintf(w, "  %s\n", c)
		}
		for _, m := range r.Manual {
			fmt.Fprintf(w, "  Run this later: %s\n", m)
		}
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  %s\n", n)
		}
		if r.Removed {
			removed = true
			if r.Bot != "" && !seen[r.Bot] {
				seen[r.Bot] = true
				bots = append(bots, r.Bot)
			}
		}
	}
	if !removed {
		return
	}
	sort.Strings(bots)
	app := "the Slack app(s)"
	if len(bots) > 0 {
		app = "the Slack app(s) " + strings.Join(bots, ", ")
	}
	fmt.Fprintf(w, "\nDelete %s at https://api.slack.com/apps if you no longer need them.\n", app)
	fmt.Fprintln(w, "Restart any running Claude/Codex sessions for these homes.")
}

// UninstallMain runs `slack-mcp-server uninstall` and returns the exit code.
func UninstallMain(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository directory (for the state file)")
	bin := fs.String("bin", "", "absolute path the binary is linked at (default: from the state file)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	user, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		return 1
	}
	linkPath := ""
	if *bin != "" {
		if linkPath, err = binPath(*bin); err != nil {
			fmt.Fprintln(os.Stderr, "uninstall:", err)
			return 2
		}
	}
	results, err := Uninstall(Options{Repo: *repo, Bin: linkPath, UserHome: user,
		P: NewTerminal(os.Stdin, os.Stdout), R: ExecRunner{}, Now: time.Now})
	printUninstallSummary(os.Stdout, results)
	if err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		return 1
	}
	return 0
}
