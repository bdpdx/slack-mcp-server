package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderAppServerUnit(t *testing.T) {
	got := string(renderAppServerUnit("/home/alice/.codex-work", "/home/alice", "/home/alice/.local/libexec/codex-app-server-supervisor", "/usr/local/bin/codex", "alice"))
	for _, want := range []string{
		"[Unit]\nDescription=Codex app-server for /home/alice/.codex-work\nStartLimitIntervalSec=0\n",
		`Environment="CODEX_HOME=/home/alice/.codex-work"`,
		`Environment="HOME=/home/alice"`,
		`Environment="LOGNAME=alice"`,
		`Environment="PATH=/home/alice/.local/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"`,
		`Environment="USER=alice"`,
		`ExecStart="/home/alice/.local/libexec/codex-app-server-supervisor" "/usr/local/bin/codex"`,
		"Restart=on-failure\nRestartSec=10\nKillMode=process\n",
		"StandardOutput=append:/home/alice/.codex-work/app-server-daemon/systemd.stdout.log",
		"StandardError=append:/home/alice/.codex-work/app-server-daemon/systemd.stderr.log",
		"[Install]\nWantedBy=default.target\n",
	} {
		assert.Contains(t, got, want)
	}
}

func TestRenderedUnitRoundTripsSpecialCharacters(t *testing.T) {
	home := `/a b/100%/$x/"q"/.codex-z`
	unit := string(renderAppServerUnit(home, "/u", `/s p/$sup%`, `/c "x"/codex`, "me"))
	assert.Contains(t, unit, `Environment="CODEX_HOME=/a b/100%%/$x/\"q\"/.codex-z"`)
	assert.Contains(t, unit, `ExecStart="/s p/$$sup%%" "/c \"x\"/codex"`)
	got, ok := unitCodexHome(unit)
	assert.True(t, ok)
	assert.Equal(t, home, got)
	assert.Equal(t, []string{`/s p/$sup%`, `/c "x"/codex`}, unitWords(unitExecRe.FindStringSubmatch(unit)[1]))
}

// serviceCalls drops the loginctl calls that every run makes.
func serviceCalls(r *fakeRunner) []string {
	var calls []string
	for _, c := range r.calls {
		if !strings.HasPrefix(c, "loginctl ") {
			calls = append(calls, c)
		}
	}
	return calls
}

type asEnv struct {
	user, home, unitPath, unit, label, supervisor, codex string
}

func newAsEnv(t *testing.T, dir string) asEnv {
	user := t.TempDir()
	home := filepath.Join(user, dir)
	require.NoError(t, os.MkdirAll(home, 0o700))
	return asEnv{user, home, appServerUnitPath(user, home), appServerUnitName(home), appServerLabel(home),
		filepath.Join(user, ".local", "libexec", "codex-app-server-supervisor"), filepath.Join(user, "bin", "codex")}
}

func (e asEnv) writeSupervisor(t *testing.T) {
	require.NoError(t, os.MkdirAll(filepath.Dir(e.supervisor), 0o755))
	require.NoError(t, os.WriteFile(e.supervisor, []byte("#!/bin/sh\n"), 0o755))
}

// writeUnit writes a unit for home whose supervisor and codex exist.
func (e asEnv) writeUnit(t *testing.T, home string) {
	e.writeSupervisor(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(e.codex), 0o755))
	require.NoError(t, os.WriteFile(e.codex, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, writeAtomic(e.unitPath, renderAppServerUnit(home, e.user, e.supervisor, e.codex, "u"), 0o644))
}

func TestAppServerUnitPath(t *testing.T) {
	assert.Equal(t, "/u/.config/systemd/user/com.openai.codex-test.app-server.service", appServerUnitPath("/u", "/u/.codex-test"))
}

func TestEnsureAppServerCreatesServiceAndSupervisor(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	r := &fakeRunner{}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Empty(t, notes)
	assert.Contains(t, changed, "app-server service "+e.label+" created and started")

	sup, err := os.ReadFile(e.supervisor)
	require.NoError(t, err)
	assert.Equal(t, supervisorScript, string(sup))
	info, _ := os.Stat(e.supervisor)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	unit, err := os.ReadFile(e.unitPath)
	require.NoError(t, err)
	assert.Contains(t, string(unit), `Environment="CODEX_HOME=`+e.home+`"`)
	assert.Contains(t, string(unit), `ExecStart="`+e.supervisor+`" "/usr/local/bin/codex"`)
	info, _ = os.Stat(e.unitPath)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	assert.DirExists(t, filepath.Join(e.home, "app-server-daemon"))

	assert.Equal(t, []string{"systemctl --user daemon-reload", "systemctl --user enable --now " + e.unit}, serviceCalls(r))
}

func TestEnsureAppServerEnablesLinger(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	r := &fakeRunner{}
	changed, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Contains(t, r.calls, "loginctl enable-linger "+currentUsername())
	assert.Contains(t, strings.Join(changed, "\n"), "lingering enabled")
}

func TestEnsureAppServerLingerAlreadyOn(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	r := &fakeRunner{scripts: map[string]fakeResult{"loginctl show-user": {"yes\n", nil}}}
	changed, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.NotContains(t, r.calls, "loginctl enable-linger "+currentUsername())
	assert.NotContains(t, strings.Join(changed, "\n"), "lingering")
}

func TestEnsureAppServerLingerFailureIsANote(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	r := &fakeRunner{scripts: map[string]fakeResult{"loginctl enable-linger": {"denied", errors.New("exit 1")}}}
	_, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Contains(t, strings.Join(notes, "\n"), "sudo loginctl enable-linger "+currentUsername())
}

func TestEnsureAppServerChecksLingerOnExistingUnit(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writeUnit(t, e.home)
	r := &fakeRunner{}
	_, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Contains(t, r.calls, "loginctl enable-linger "+currentUsername())
}

func TestEnsureAppServerReusesCurrentSupervisor(t *testing.T) {
	e := newAsEnv(t, ".codex")
	require.NoError(t, os.MkdirAll(filepath.Dir(e.supervisor), 0o755))
	require.NoError(t, os.WriteFile(e.supervisor, []byte(supervisorScript), 0o755))
	r := &fakeRunner{}
	changed, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.NotContains(t, changed, e.supervisor, "current supervisor not rewritten")
	assert.FileExists(t, e.unitPath)
	assert.Len(t, serviceCalls(r), 2)
}

func TestEnsureAppServerReplacesStaleSupervisor(t *testing.T) {
	e := newAsEnv(t, ".codex")
	e.writeSupervisor(t)
	r := &fakeRunner{}
	changed, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Contains(t, changed, e.supervisor)
	sup, _ := os.ReadFile(e.supervisor)
	assert.Equal(t, supervisorScript, string(sup))
}

func TestEnsureAppServerSkippedWithoutCodex(t *testing.T) {
	e := newAsEnv(t, ".codex")
	r := &fakeRunner{missing: map[string]bool{"codex": true}}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"Codex CLI not found; install Codex, then run ./install.sh again to set up its app-server"}, notes)
	assert.NoFileExists(t, e.unitPath)
	assert.Empty(t, r.calls)
}

func TestEnsureAppServerEnableFailure(t *testing.T) {
	e := newAsEnv(t, ".codex")
	r := &fakeRunner{scripts: map[string]fakeResult{"systemctl --user enable": {"boom", errors.New("exit 5")}}}
	_, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	assert.ErrorContains(t, err, "systemctl --user enable --now")
}

func TestEnsureAppServerExistingRunning(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writeUnit(t, e.home)
	before, _ := os.ReadFile(e.unitPath)
	r := &fakeRunner{scripts: map[string]fakeResult{"loginctl show-user": {"yes\n", nil}}}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"app-server " + e.label + " verified and running"}, notes)
	assert.Equal(t, []string{"systemctl --user is-active " + e.unit}, serviceCalls(r))
	after, _ := os.ReadFile(e.unitPath)
	assert.Equal(t, before, after)
}

func TestEnsureAppServerNotRunningYesStarts(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writeUnit(t, e.home)
	r := &fakeRunner{scripts: map[string]fakeResult{"systemctl --user is-active": {"inactive\n", errors.New("exit 3")}, "loginctl show-user": {"yes\n", nil}}}
	p := &Scripted{Answers: []string{"y"}}
	changed, _, err := ensureAppServer(e.home, e.user, r, p)
	require.NoError(t, err)
	assert.Equal(t, []string{"app-server " + e.label + " started"}, changed)
	assert.Equal(t, []string{"systemctl --user is-active " + e.unit, "systemctl --user daemon-reload", "systemctl --user enable --now " + e.unit}, serviceCalls(r))
}

func TestEnsureAppServerNotRunningNo(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writeUnit(t, e.home)
	r := &fakeRunner{scripts: map[string]fakeResult{"systemctl --user is-active": {"inactive\n", errors.New("exit 3")}, "loginctl show-user": {"yes\n", nil}}}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{Answers: []string{"n"}})
	require.NoError(t, err)
	assert.Empty(t, changed)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "is not running; Slack messages won't reach this Codex home")
	assert.Equal(t, []string{"systemctl --user is-active " + e.unit}, serviceCalls(r))
}

func TestEnsureAppServerMismatchLeavesUnit(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writeUnit(t, "/somewhere/else")
	before, _ := os.ReadFile(e.unitPath)
	r := &fakeRunner{}
	_, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	require.NotEmpty(t, notes)
	all := strings.Join(notes, "\n")
	assert.Contains(t, all, "app-server unit "+e.unitPath+": CODEX_HOME is \"/somewhere/else\", expected")
	assert.Contains(t, all, "(left unchanged)")
	assert.Contains(t, all, "is running, but its unit differs (see above)")
	assert.NotContains(t, all, "verified and running")
	after, _ := os.ReadFile(e.unitPath)
	assert.Equal(t, before, after)
}

func TestVerifyAppServerUnitDefaultHomeWithoutCodexHome(t *testing.T) {
	e := newAsEnv(t, ".codex")
	e.writeUnit(t, e.home)
	rendered, _ := os.ReadFile(e.unitPath)
	unit := strings.Replace(string(rendered), `Environment="CODEX_HOME=`+e.home+`"`+"\n", "", 1)
	assert.Empty(t, verifyAppServerUnit(unit, e.home, e.user))
	assert.NotEmpty(t, verifyAppServerUnit(unit, filepath.Join(e.user, ".codex-x"), e.user))
	assert.NotEmpty(t, verifyAppServerUnit(strings.ReplaceAll(unit, e.supervisor, "/no/such"), e.home, e.user))
	assert.Equal(t, []string{`codex /no/codex does not exist`}, verifyAppServerUnit(strings.ReplaceAll(unit, e.codex, "/no/codex"), e.home, e.user))
	noArg := strings.Replace(unit, ` "`+e.codex+`"`, "", 1)
	assert.Empty(t, verifyAppServerUnit(noArg, e.home, e.user), "unit without the codex argument is still valid")
	assert.Equal(t, []string{"no program in ExecStart"}, verifyAppServerUnit("[Service]\n", e.home, e.user))
}

func TestVerifyAppServerUnitUnquoted(t *testing.T) {
	e := newAsEnv(t, ".codex-x")
	e.writeUnit(t, e.home)
	unit := "Environment=CODEX_HOME=" + e.home + "\nExecStart=" + e.supervisor + " " + e.codex + "\n"
	assert.Empty(t, verifyAppServerUnit(unit, e.home, e.user), "a hand-written unit need not quote")
}

func TestEnsureAppServerMismatchNotRunningDoesNotOffer(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writeUnit(t, "/somewhere/else")
	r := &fakeRunner{scripts: map[string]fakeResult{"systemctl --user is-active": {"inactive\n", errors.New("exit 3")}, "loginctl show-user": {"yes\n", nil}}}
	p := &Scripted{} // no answers: a prompt would abort
	changed, notes, err := ensureAppServer(e.home, e.user, r, p)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Contains(t, strings.Join(notes, "\n"), "fix or remove the unit above and run ./install.sh again")
	assert.Equal(t, []string{"systemctl --user is-active " + e.unit}, serviceCalls(r))
	assert.Empty(t, p.Out.String())
}

func TestClipboardNeedsADisplayAndATool(t *testing.T) {
	env := func(vals map[string]string) func(string) string { return func(k string) string { return vals[k] } }
	have := func(tools ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, t := range tools {
				if t == n {
					return "/usr/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	assert.Nil(t, clipboard("linux", env(nil), have("xclip", "wl-copy")), "no display, as over SSH")
	assert.Nil(t, clipboard("linux", env(map[string]string{"DISPLAY": ":0"}), have()), "no tool")
	assert.NotNil(t, clipboard("linux", env(map[string]string{"DISPLAY": ":0"}), have("xsel")))
	assert.NotNil(t, clipboard("linux", env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}), have("wl-copy")))
	assert.NotNil(t, clipboard("darwin", env(nil), have("pbcopy")), "macOS needs no display variable")
}
