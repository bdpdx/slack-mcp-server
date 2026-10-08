package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Options configures Run.
type Options struct {
	Repo, Bin, UserHome string
	P                   Prompter
	V                   Validator
	R                   Runner
	Now                 func() time.Time
	// Clipboard copies text for the user (pbcopy); nil means don't offer.
	Clipboard func([]byte) error
}

const (
	actUpdate = iota
	actReinstall
	actSkip
)

const (
	uniqueNote  = "Bot names must be unique in this Slack workspace. Confirm the name with your Slack workspace admin before creating the app."
	appTokHelp  = "App-level token (xapp-…): Settings → Basic Information → App-Level Tokens → Generate Token and Scopes, add the scope connections:write. Slack shows it only once; if you lost it, generate a new one."
	userMsg     = "Your Slack username %q contains _ or ., which this setup's channel names use as separators. Ask your workspace admin to change it, then run setup again."
	takenMsg    = "Warning: another bot in this workspace is already named %q. Bot names must be unique; consider reinstalling this home with a different name."
	iconMsgFmt  = "Set an icon for %s: https://app.slack.com/apps → Build → %s → Settings → Basic Information → Display Information."
	tokenHelpFm = "Bot token (xoxb-…) and user token (xoxp-…): https://app.slack.com/apps → Build → %s → Settings → Features → OAuth & Permissions → OAuth Tokens. If tokens do not appear click \"Install to <Workspace>.\""
)

// Run drives the interactive setup. ErrAborted ends it immediately.
func Run(ctx context.Context, o Options) ([]Result, error) {
	statePath := filepath.Join(o.Repo, ".install-state.json")
	st, err := LoadState(statePath)
	if err != nil {
		return nil, err
	}
	st.Bin = o.Bin
	homes, err := chooseHomes(o, st)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, h := range homes {
		res, done, err := processHome(ctx, o, st, h)
		if errors.Is(err, ErrAborted) {
			return results, err
		}
		if err != nil {
			redo := "choose Update for this home"
			var se *stepError
			if errors.As(err, &se) {
				redo = se.redo
			}
			res.Notes = append(res.Notes, fmt.Sprintf("FAILED: %s. Fix this, then run ./install.sh again and %s.", strings.TrimRight(err.Error(), "."), redo))
		}
		res.Type, res.Installed = h.Type, done
		if done {
			if serr := st.Save(statePath); serr != nil {
				return results, serr
			}
		}
		if res.Home == "" {
			res.Home = h.Path
		}
		results = append(results, res)
	}
	return results, nil
}

func chooseHomes(o Options, st *State) ([]Home, error) {
	for _, path := range st.ForgetMissing() {
		o.P.Say("Forgetting %s: the folder no longer exists", path)
	}
	homes := DiscoverHomes(o.UserHome, st)
	for {
		o.P.Say("Agent homes found:")
		for i, h := range homes {
			env := ""
			if h.HasEnv {
				env = " (has .env)"
			}
			o.P.Say("  %d. %s [%s]%s", i+1, h.Path, h.Type, env)
		}
		i, err := o.P.Choose("What next?", []string{"Set up these homes", "Add a home", "Remove a home", "Change a home's path"}, 0)
		if err != nil {
			return nil, err
		}
		switch i {
		case 0:
			return homes, st.Save(filepath.Join(o.Repo, ".install-state.json"))
		case 1:
			h, err := addHome(o, homes)
			if err != nil {
				return nil, err
			}
			homes = append(homes, h)
		case 2, 3:
			if len(homes) == 0 {
				o.P.Say("There are no homes yet.")
				continue
			}
			if homes, err = editHome(o, st, homes, i == 2); err != nil {
				return nil, err
			}
		}
	}
}

func addHome(o Options, homes []Home) (Home, error) {
	var path string
	for path == "" {
		raw, err := o.P.Ask("Path of the home", "")
		if err != nil {
			return Home{}, err
		}
		if raw == "" {
			o.P.Say("A path is required.")
			continue
		}
		if path, err = ExpandPath(raw, o.UserHome); err != nil {
			return Home{}, err
		}
		for _, h := range homes {
			if h.Path == path {
				o.P.Say("%s is already in the list.", path)
				path = ""
			}
		}
	}
	typ := DetectType(path)
	if typ == "" {
		i, err := o.P.Choose("Which agent uses this home?", []string{"Claude", "Codex"}, 0)
		if err != nil {
			return Home{}, err
		}
		typ = []string{TypeClaude, TypeCodex}[i]
	}
	return Home{Path: path, Type: typ, HasEnv: exists(EnvPath(path))}, nil
}

func editHome(o Options, st *State, homes []Home, remove bool) ([]Home, error) {
	opts := make([]string, len(homes))
	for i, h := range homes {
		opts[i] = h.Path
	}
	q := "Which home do you want to change?"
	if remove {
		q = "Which home do you want to remove?"
	}
	i, err := o.P.Choose(q, opts, 0)
	if err != nil {
		return nil, err
	}
	old := homes[i]
	if remove {
		dropState(st, old.Path)
		return append(homes[:i:i], homes[i+1:]...), nil
	}
	raw, err := o.P.Ask("New path", old.Path)
	if err != nil {
		return nil, err
	}
	path, err := ExpandPath(raw, o.UserHome)
	if err != nil {
		return nil, err
	}
	dropState(st, old.Path)
	typ := DetectType(path)
	if typ == "" {
		typ = old.Type
	}
	homes[i] = Home{Path: path, Type: typ, HasEnv: exists(EnvPath(path))}
	return homes, nil
}

func dropState(st *State, path string) {
	var keep []HomeState
	for _, h := range st.Homes {
		if h.Path != path {
			keep = append(keep, h)
		}
	}
	st.Homes = keep
}

// processHome handles one home; done reports that state should be saved.
func processHome(ctx context.Context, o Options, st *State, h Home) (Result, bool, error) {
	action := -1
	bot := ""
	if hs := st.Home(h.Path); hs != nil {
		bot = hs.Bot
	}
	if h.HasEnv {
		a, err := o.P.Choose(fmt.Sprintf("%s already has a slack-mcp-server.env.", h.Path),
			[]string{"Update (keep tokens, refresh config)", "Reinstall (new tokens)", "Skip"}, 0)
		if err != nil {
			return Result{}, false, err
		}
		action = a
		if action == actSkip {
			res := Result{Home: h.Path, Notes: []string{"Skipped."}}
			if old := ExistingBin([]Home{h}); old != "" && old != o.Bin {
				res.Notes = append(res.Notes, fmt.Sprintf("This home still points at the old binary path %s; run setup and choose Update to relink it to %s.", old, o.Bin))
			}
			return res, false, nil
		}
	}
	var notes []string
	if action != actUpdate {
		name, n, err := configureEnv(ctx, o, h, bot)
		if err != nil {
			if errors.Is(err, ErrAborted) {
				return Result{Home: h.Path}, false, err
			}
			redo := "set up this home again"
			if h.HasEnv {
				redo = "choose Reinstall for this home"
			}
			return Result{Home: h.Path}, false, &stepError{err: err, redo: redo}
		}
		if name == "" { // declined to overwrite
			return Result{Home: h.Path, Notes: []string{"Skipped."}}, false, nil
		}
		bot, notes = name, n
	}
	oldBin := ExistingBin([]Home{h})
	before := snapshotSessionFiles(h.Path)
	res, err := installHome(o, h)
	res.Fresh = action != actUpdate
	// Running sessions need a restart only if what they read at start
	// actually differs now. Setup's writes alone don't say so: re-registering
	// the MCP server rewrites config.toml and setup then restores it, and a
	// JSON rewrite can only reorder keys.
	after := snapshotSessionFiles(h.Path)
	res.Stale = oldBin != "" && oldBin != o.Bin // its sessions' MCP server still runs the old path
	for name, was := range before {
		if !sameContent(name, was, after[name]) {
			res.Stale = true
		}
		if name == "hooks.json" && !bytes.Equal(was, after[name]) {
			res.Hooks = true // Codex asks to trust hooks whose file changed at all
		}
	}
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return res, false, err
	}
	st.SetHome(HomeState{Path: h.Path, Type: h.Type, Bot: bot})
	return res, true, nil
}

// stepError is a home's failure and the setup choice that redoes it.
type stepError struct {
	err  error
	redo string
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

func installHome(o Options, h Home) (Result, error) {
	if h.Type == TypeCodex {
		return InstallCodex(h.Path, o.UserHome, o.Bin, o.R, o.P, o.Now())
	}
	return InstallClaude(h.Path, o.UserHome, o.Bin, o.R, o.Now())
}

// configureEnv does bot setup and writes the env file. It returns an empty
// name when the user declines to overwrite an existing file.
func configureEnv(ctx context.Context, o Options, h Home, saved string) (string, []string, error) {
	envPath := EnvPath(h.Path)
	if h.HasEnv {
		ok, err := o.P.Confirm("Overwrite the existing slack-mcp-server.env?", true)
		if err != nil || !ok {
			return "", nil, err
		}
	}
	name, tok, err := setupBot(ctx, o, h, saved)
	if err != nil {
		return "", nil, err
	}
	existing, err := ReadEnv(envPath)
	if err != nil {
		return "", nil, err
	}
	tools := DefaultTools(existing)
	reportTools(o.P, envPath, tools, CustomTools(existing))
	data, err := RenderEnv(MergeEnv(existing, tok, tools))
	if err != nil {
		return "", nil, err
	}
	if _, err := replaceFile(envPath, data, 0o600, o.Now()); err != nil {
		return "", nil, err
	}
	return name, []string{fmt.Sprintf(iconMsgFmt, name, name)}, nil
}

// reportTools says which tool settings were written: permissive defaults
// (deleting messages and Slack Connect invites stay off), existing values
// kept, custom channel lists untouched. Nothing is asked; the user edits the
// .env to change them.
func reportTools(p Prompter, envPath string, tools map[string]bool, custom map[string]string) {
	var on, off []string
	for k, v := range tools {
		if v {
			on = append(on, k)
		} else {
			off = append(off, k)
		}
	}
	sort.Strings(on)
	sort.Strings(off)
	p.Say("\nTools enabled: %s", strings.Join(on, ", "))
	if len(off) > 0 {
		p.Say("Tools off: %s", strings.Join(off, ", "))
	}
	if len(custom) > 0 {
		keys := make([]string, 0, len(custom))
		for k := range custom {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		p.Say("These tool settings have custom values and are kept as they are:")
		for _, k := range keys {
			p.Say("  %s=%s: custom (kept)", k, custom[k])
		}
	}
	p.Say("To change them, edit %s and restart the agent.", envPath)
}

func setupBot(ctx context.Context, o Options, h Home, saved string) (string, Tokens, error) {
	p := o.P
	p.Say("%s", uniqueNote)
	def := saved
	if def == "" {
		def = h.Type
	}
	var name string
	for {
		n, err := p.Ask("Bot name", def)
		if err != nil {
			return "", Tokens{}, err
		}
		if problem := ValidateBotName(n); problem != "" {
			p.Say("%s", problem)
			continue
		}
		name = n
		break
	}
	if err := showManifest(o, name); err != nil {
		return "", Tokens{}, err
	}
	p.Say("\n"+iconMsgFmt, name, name)
	tok, err := askTokens(ctx, o, name)
	return name, tok, err
}

// showManifest saves the Slack app manifest, offers to copy it, and says how
// to create the app from it (or update an existing app to match).
func showManifest(o Options, name string) error {
	m, err := RenderManifest(name)
	if err != nil {
		return err
	}
	path := filepath.Join(o.Repo, ".install", "manifests", name+".json")
	if err := writeAtomic(path, m, 0o600); err != nil {
		return err
	}
	p := o.P
	p.Say("\nSlack app manifest for %s saved to %s", name, path)
	if o.Clipboard != nil {
		copyIt, err := p.Confirm("Copy the manifest to the clipboard?", true)
		if err != nil {
			return err
		}
		if copyIt {
			if err := o.Clipboard(m); err != nil {
				p.Say("Could not copy it (%v); open the file instead.", err)
			} else {
				p.Say("Copied to the clipboard.")
			}
		}
	}
	p.Say("\nNew app: open https://api.slack.com/apps, click Create New App → From a manifest, choose your workspace, paste the manifest, click Create, then Install to Workspace.")
	p.Say("\nExisting app: https://app.slack.com/apps → Build → %s → Settings → App Manifest, paste the manifest, Save Changes, and reinstall the app if Slack asks.", name)
	_, err = p.Ask("\nPress Enter when the app is installed", "")
	return err
}

// askToken reads one token until it is non-empty and has the right prefix.
func askToken(p Prompter, kind, label string) (string, error) {
	for {
		p.Say("")
		s, err := p.Secret(label)
		if err != nil {
			return "", err
		}
		s = NormalizeToken(s)
		if s == "" {
			p.Say("A %s token is required.", kind)
			continue
		}
		if problem := CheckTokenPrefix(kind, s); problem != "" {
			p.Say("%s", problem)
			continue
		}
		return s, nil
	}
}

func askTokens(ctx context.Context, o Options, name string) (Tokens, error) {
	p := o.P
	p.Say("\n%s", appTokHelp)
	p.Say("\n"+tokenHelpFm, name)
	var tok Tokens
	need := map[string]bool{"app": true, "bot": true, "user": true}
	for {
		var err error
		for _, k := range []struct {
			kind, label string
			dst         *string
		}{
			{"app", "App-level token (xapp-…)", &tok.App},
			{"bot", "Bot token (xoxb-…)", &tok.Bot},
			{"user", "User token (xoxp-…)", &tok.User},
		} {
			if need[k.kind] {
				if *k.dst, err = askToken(p, k.kind, k.label); err != nil {
					return Tokens{}, err
				}
				need[k.kind] = false
			}
		}
		redo, err := validateTokens(ctx, o, name, tok)
		if err != nil {
			return Tokens{}, err
		}
		if redo == "" {
			return tok, nil
		}
		need[redo] = true
	}
}

// validateTokens checks the tokens live. It returns the kind to re-ask, or
// an error that ends this home.
func validateTokens(ctx context.Context, o Options, name string, tok Tokens) (string, error) {
	p := o.P
	bot, err := o.V.AuthTest(ctx, tok.Bot)
	if err != nil {
		p.Say("The bot token was rejected by Slack: %v. Please enter the bot token again.", err)
		return "bot", nil
	}
	if !bot.IsBot {
		p.Say("That token does not belong to a bot. Please enter the bot token (xoxb-…) again.")
		return "bot", nil
	}
	user, err := o.V.AuthTest(ctx, tok.User)
	if err != nil {
		p.Say("The user token was rejected by Slack: %v. Please enter the user token again.", err)
		return "user", nil
	}
	if user.IsBot {
		p.Say("That token belongs to a bot, not a person. Please enter the user token (xoxp-…) again.")
		return "user", nil
	}
	if user.TeamID != bot.TeamID {
		p.Say("The user token is for a different Slack workspace than the bot token. Please enter the user token again.")
		return "user", nil
	}
	if strings.ContainsAny(user.User, "_.") {
		return "", fmt.Errorf(userMsg, user.User)
	}
	if err := o.V.CheckAppToken(ctx, tok.App); err != nil {
		p.Say("The app-level token was rejected by Slack: %v. Please enter the app-level token again.", err)
		return "app", nil
	}
	taken, err := o.V.BotNameTaken(ctx, tok.Bot, name, bot.UserID)
	switch {
	case err != nil:
		p.Say("Could not check whether %q is already used by another bot: %v", name, err)
	case taken:
		p.Say(takenMsg, name)
	}
	return "", nil
}

// existingBinFor returns the binary path installed hooks already use, or "".
func existingBinFor(user, repo string) string {
	st, err := LoadState(filepath.Join(repo, ".install-state.json"))
	if err != nil {
		st = &State{}
	}
	return ExistingBin(DiscoverHomes(user, st))
}

// binPath makes the --bin value absolute; empty means this executable.
func binPath(bin string) (string, error) {
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("--bin is required (%v)", err)
		}
		bin = exe
	}
	return filepath.Abs(bin)
}

// Main runs `slack-mcp-server setup` and returns the exit code.
func Main(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository directory (for the state file and manifests)")
	bin := fs.String("bin", "", "absolute path the binary is linked at")
	printBin := fs.Bool("print-existing-bin", false, "print the binary path already used by installed hooks, then exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *printBin {
		user, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			return 1
		}
		fmt.Println(existingBinFor(user, *repo))
		return 0
	}
	linkPath, err := binPath(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 2
	}
	user, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 1
	}
	results, err := Run(context.Background(), Options{
		Repo: *repo, Bin: linkPath, UserHome: user,
		P: NewTerminal(os.Stdin, os.Stdout), V: SlackValidator{}, R: ExecRunner{}, Now: time.Now,
		Clipboard: pbcopy,
	})
	printSummary(os.Stdout, linkPath, results, errors.Is(err, ErrAborted))
	code := 0
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		code = 1
	}
	st, serr := LoadState(filepath.Join(*repo, ".install-state.json"))
	if serr != nil {
		fmt.Fprintln(os.Stderr, "setup: listeners were not restarted:", serr)
		return 1
	}
	if RestartListeners(os.Stdout, ExecRunner{}, linkPath, DiscoverHomes(user, st)) {
		fmt.Fprintln(os.Stderr, "setup: a listener restart failed; see Listeners above")
		code = 1
	}
	return code
}

// printSummary reports each home, then the remaining manual steps when at
// least one home was set up and setup was not aborted.
// pbcopy puts data on the macOS clipboard.
func pbcopy(data []byte) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

func printSummary(w io.Writer, bin string, results []Result, aborted bool) {
	fmt.Fprintln(w, "\nSummary")
	if real, err := filepath.EvalSymlinks(bin); err == nil && real != bin {
		fmt.Fprintf(w, "\nslack-mcp-server is installed at %s (a link to %s)\n", bin, real)
	} else {
		fmt.Fprintf(w, "\nslack-mcp-server is installed at %s\n", bin)
	}
	installed, fresh, codex, icon := false, false, false, false
	var stale []string // updated homes whose running sessions need a restart
	for _, r := range results {
		fmt.Fprintf(w, "\n%s\n", r.Home)
		for _, c := range r.Changed {
			fmt.Fprintf(w, "  changed: %s\n", c)
		}
		for _, m := range r.Manual {
			fmt.Fprintf(w, "  Run this later: %s\n", m)
		}
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  %s\n", n)
			icon = icon || (r.Installed && strings.HasPrefix(n, "Set an icon for "))
		}
		installed = installed || r.Installed
		fresh = fresh || (r.Installed && r.Fresh)
		if r.Installed && !r.Fresh && r.Stale {
			stale = append(stale, r.Home)
		}
		codex = codex || (r.Installed && (r.Fresh || r.Hooks) && r.Type == TypeCodex)
	}
	if aborted {
		fmt.Fprintln(w, "\nSetup stopped before it finished. Run ./install.sh again to set up the remaining homes.")
		return
	}
	if !installed {
		return
	}
	// An update needs no restarts: setup restarts each running listener on
	// the new binary itself (see Listeners). Only a home set up new or
	// reinstalled with new tokens, or one whose hooks or Slack MCP settings
	// changed (sessions read those when they start), needs its sessions
	// started fresh.
	var steps []string
	if icon {
		steps = append(steps, "Set the bot icon (see the icon notes above).")
	}
	if fresh {
		steps = append(steps, "Start (or restart) agent sessions in the homes set up or reinstalled above.")
	}
	if len(stale) > 0 {
		steps = append(steps, fmt.Sprintf("Restart agent sessions in %s: setup changed hooks or Slack MCP settings, which running sessions read only when they start.", strings.Join(stale, ", ")))
	}
	if codex {
		steps = append(steps, "Trust the hooks when Codex asks.")
	}
	if fresh {
		steps = append(steps, "Tell the agent: start a project chat called <name>.")
	}
	if len(steps) == 0 {
		return
	}
	fmt.Fprintln(w, "\nRemaining steps:")
	for i, s := range steps {
		fmt.Fprintf(w, "  %d. %s\n", i+1, s)
	}
}

// sessionFiles are the files in a home that agent sessions read when they
// start: hooks and MCP settings.
var sessionFiles = []string{"settings.json", "hooks.json", "config.toml", "rules/default.rules"}

// snapshotSessionFiles reads home's session files (nil for a missing one).
func snapshotSessionFiles(home string) map[string][]byte {
	snap := map[string][]byte{}
	for _, name := range sessionFiles {
		data, _ := os.ReadFile(filepath.Join(home, name))
		snap[name] = data
	}
	return snap
}

// sameContent reports whether two versions of a session file mean the same
// thing: JSON compared by value (key order and formatting don't matter),
// anything else byte for byte.
func sameContent(name string, a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	if !strings.HasSuffix(name, ".json") || len(a) == 0 || len(b) == 0 {
		return false
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
