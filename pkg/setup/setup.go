package setup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	tokenHelpFm = "Bot token (xoxb-…) and user token (xoxp-…): https://app.slack.com/apps → Build → %s → Settings → Features → OAuth & Permissions → OAuth Tokens."
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
			res.Home = h.Path
			res.Notes = append(res.Notes, "FAILED: "+err.Error())
		}
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
			return Result{Home: h.Path}, false, err
		}
		if name == "" { // declined to overwrite
			return Result{Home: h.Path, Notes: []string{"Skipped."}}, false, nil
		}
		bot, notes = name, n
	}
	res, err := installHome(o, h)
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return res, false, err
	}
	st.SetHome(HomeState{Path: h.Path, Type: h.Type, Bot: bot})
	return res, true, nil
}

func installHome(o Options, h Home) (Result, error) {
	if h.Type == TypeCodex {
		return InstallCodex(h.Path, o.Bin, o.R, o.P, o.Now())
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
	tools, err := askTools(o.P, DefaultTools(existing), CustomTools(existing))
	if err != nil {
		return "", nil, err
	}
	data, err := RenderEnv(MergeEnv(existing, tok, tools))
	if err != nil {
		return "", nil, err
	}
	if _, err := replaceFile(envPath, data, 0o600, o.Now()); err != nil {
		return "", nil, err
	}
	return name, []string{fmt.Sprintf(iconMsgFmt, name, name)}, nil
}

func askTools(p Prompter, tools map[string]bool, custom map[string]string) (map[string]bool, error) {
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
	var on []string
	for k, v := range tools {
		if v {
			on = append(on, k)
		}
	}
	sort.Strings(on)
	ok, err := p.Confirm(fmt.Sprintf("Enable the default tools (%s)?", strings.Join(on, ", ")), true)
	if err != nil || ok {
		return tools, err
	}
	keys := make([]string, 0, len(tools))
	for k := range tools {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]bool{}
	for _, k := range keys {
		if out[k], err = p.Confirm("Enable "+k+"?", tools[k]); err != nil {
			return nil, err
		}
	}
	return out, nil
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
	appExists, err := p.Confirm("Does the Slack app already exist?", false)
	if err != nil {
		return "", Tokens{}, err
	}
	if !appExists {
		if err := showManifest(o, name); err != nil {
			return "", Tokens{}, err
		}
	}
	p.Say(iconMsgFmt, name, name)
	tok, err := askTokens(ctx, o, name)
	return name, tok, err
}

func showManifest(o Options, name string) error {
	m, err := RenderManifest(name)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(o.Repo, ".install", "manifests", name+".json"), m, 0o600); err != nil {
		return err
	}
	o.P.Say("%s", string(m))
	o.P.Say("1. Open https://api.slack.com/apps and click Create New App → From a manifest.")
	o.P.Say("2. Choose your workspace, paste the manifest above, and click Create.")
	o.P.Say("3. Click Install to Workspace and allow.")
	_, err = o.P.Ask("Press Enter when the app is installed", "")
	return err
}

// askToken reads one token until it is non-empty and has the right prefix.
func askToken(p Prompter, kind, label string) (string, error) {
	for {
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
	p.Say("%s", appTokHelp)
	p.Say(tokenHelpFm, name)
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
	if *bin == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "setup: --bin is required")
			return 2
		}
		*bin = exe
	}
	user, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 1
	}
	results, err := Run(context.Background(), Options{
		Repo: *repo, Bin: *bin, UserHome: user,
		P: NewTerminal(os.Stdin, os.Stdout), V: SlackValidator{}, R: ExecRunner{}, Now: time.Now,
	})
	printSummary(os.Stdout, results)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 1
	}
	return 0
}

func printSummary(w io.Writer, results []Result) {
	fmt.Fprintln(w, "\nSummary")
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
		}
	}
	fmt.Fprintln(w, "\nRemaining steps:")
	fmt.Fprintln(w, "  1. Set the bot icon (see the icon notes above).")
	fmt.Fprintln(w, "  2. Restart your agent sessions.")
	fmt.Fprintln(w, "  3. Trust the hooks when Codex asks.")
	fmt.Fprintln(w, "  4. Tell the agent: start a project chat called <name>.")
}
