package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppServerLabel(t *testing.T) {
	assert.Equal(t, "com.openai.codex-test.app-server", appServerLabel("/u/.codex-test"))
	assert.Equal(t, "com.openai.codex.app-server", appServerLabel("/u/.codex"))
	assert.Equal(t, "com.openai.codex-x.app-server", appServerLabel("/u/codex-x"))
}

func TestRenderAppServerPlist(t *testing.T) {
	got := string(renderAppServerPlist("/Users/brian/.codex-rezilient", "/Users/brian", "/Users/brian/.local/libexec/codex-app-server-supervisor", "brian"))
	for _, want := range []string{
		"<key>CODEX_HOME</key>\n\t\t<string>/Users/brian/.codex-rezilient</string>",
		"<key>HOME</key>\n\t\t<string>/Users/brian</string>",
		"<key>LOGNAME</key>\n\t\t<string>brian</string>",
		"<string>/Users/brian/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
		"<key>USER</key>\n\t\t<string>brian</string>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>Label</key>\n\t<string>com.openai.codex-rezilient.app-server</string>",
		"<key>ProcessType</key>\n\t<string>Background</string>",
		"<array>\n\t\t<string>/Users/brian/.local/libexec/codex-app-server-supervisor</string>\n\t</array>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<string>/Users/brian/.codex-rezilient/app-server-daemon/launchagent.stderr.log</string>",
		"<string>/Users/brian/.codex-rezilient/app-server-daemon/launchagent.stdout.log</string>",
		"<key>ThrottleInterval</key>\n\t<integer>10</integer>",
	} {
		assert.Contains(t, got, want)
	}
	assert.True(t, strings.HasPrefix(got, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist"))

	esc := string(renderAppServerPlist("/a&b/.codex", "/a&b", "/a&b/sup", "me"))
	assert.Contains(t, esc, "<string>/a&amp;b/.codex</string>")
	assert.NotContains(t, esc, "a&b")
}

type asEnv struct {
	user, home, plist, label, target, domain, supervisor string
}

func newAsEnv(t *testing.T, dir string) asEnv {
	user := t.TempDir()
	home := filepath.Join(user, dir)
	require.NoError(t, os.MkdirAll(home, 0o700))
	label := appServerLabel(home)
	domain := "gui/" + strconv.Itoa(os.Getuid())
	return asEnv{user, home, filepath.Join(user, "Library", "LaunchAgents", label+".plist"), label, domain + "/" + label, domain,
		filepath.Join(user, ".local", "libexec", "codex-app-server-supervisor")}
}

func (e asEnv) writeSupervisor(t *testing.T) {
	require.NoError(t, os.MkdirAll(filepath.Dir(e.supervisor), 0o755))
	require.NoError(t, os.WriteFile(e.supervisor, []byte("#!/bin/sh\n"), 0o755))
}

func (e asEnv) writePlist(t *testing.T, home string) {
	e.writeSupervisor(t)
	require.NoError(t, writeAtomic(e.plist, renderAppServerPlist(home, e.user, e.supervisor, "u"), 0o644))
}

func TestEnsureAppServerCreatesAgentAndSupervisor(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	r := &fakeRunner{}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{}, testNow)
	require.NoError(t, err)
	assert.Empty(t, notes)
	assert.Contains(t, changed, "app-server launch agent "+e.label+" created and started")

	sup, err := os.ReadFile(e.supervisor)
	require.NoError(t, err)
	assert.Contains(t, string(sup), `CODEX_BIN="/usr/local/bin/codex"`)
	info, _ := os.Stat(e.supervisor)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	plist, err := os.ReadFile(e.plist)
	require.NoError(t, err)
	assert.Contains(t, string(plist), "<string>"+e.home+"</string>")
	info, _ = os.Stat(e.plist)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	assert.DirExists(t, filepath.Join(e.home, "app-server-daemon"))

	assert.Equal(t, []string{"launchctl enable " + e.target, "launchctl bootstrap " + e.domain + " " + e.plist}, r.calls)
}

func TestEnsureAppServerReusesSupervisor(t *testing.T) {
	e := newAsEnv(t, ".codex")
	e.writeSupervisor(t)
	r := &fakeRunner{missing: map[string]bool{"codex": true}}
	_, _, err := ensureAppServer(e.home, e.user, r, &Scripted{}, testNow)
	require.NoError(t, err)
	sup, _ := os.ReadFile(e.supervisor)
	assert.Equal(t, "#!/bin/sh\n", string(sup), "existing supervisor untouched")
	assert.FileExists(t, e.plist)
	assert.Len(t, r.calls, 2)
}

func TestEnsureAppServerSkippedWithoutCodex(t *testing.T) {
	e := newAsEnv(t, ".codex")
	r := &fakeRunner{missing: map[string]bool{"codex": true}}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{}, testNow)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"Codex CLI not found; install Codex, then run ./install.sh again to set up its app-server"}, notes)
	assert.NoFileExists(t, e.plist)
	assert.Empty(t, r.calls)
}

func TestEnsureAppServerBootstrapFailure(t *testing.T) {
	e := newAsEnv(t, ".codex")
	r := &fakeRunner{scripts: map[string]fakeResult{"launchctl bootstrap": {"boom", errors.New("exit 5")}}}
	_, _, err := ensureAppServer(e.home, e.user, r, &Scripted{}, testNow)
	assert.ErrorContains(t, err, "launchctl bootstrap")
}

func TestEnsureAppServerExistingRunning(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writePlist(t, e.home)
	before, _ := os.ReadFile(e.plist)
	r := &fakeRunner{scripts: map[string]fakeResult{"launchctl print " + e.target: {"foo = bar\n\tstate = running\n", nil}}}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{}, testNow)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"app-server " + e.label + " verified and running"}, notes)
	assert.Equal(t, []string{"launchctl print " + e.target}, r.calls)
	after, _ := os.ReadFile(e.plist)
	assert.Equal(t, before, after)
}

func TestEnsureAppServerNotRunningYesKickstartsLoaded(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writePlist(t, e.home)
	r := &fakeRunner{scripts: map[string]fakeResult{"launchctl print": {"state = waiting\n", nil}}}
	p := &Scripted{Answers: []string{"y"}}
	changed, _, err := ensureAppServer(e.home, e.user, r, p, testNow)
	require.NoError(t, err)
	assert.Equal(t, []string{"app-server " + e.label + " started"}, changed)
	assert.Equal(t, []string{"launchctl print " + e.target, "launchctl enable " + e.target, "launchctl kickstart -k " + e.target}, r.calls)
}

func TestEnsureAppServerNotRunningYesBootstrapsUnloaded(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writePlist(t, e.home)
	r := &fakeRunner{scripts: map[string]fakeResult{"launchctl print": {"not found", errors.New("exit 113")}}}
	p := &Scripted{Answers: []string{"y"}}
	_, _, err := ensureAppServer(e.home, e.user, r, p, testNow)
	require.NoError(t, err)
	assert.Equal(t, []string{"launchctl print " + e.target, "launchctl enable " + e.target, "launchctl bootstrap " + e.domain + " " + e.plist}, r.calls)
}

func TestEnsureAppServerNotRunningNo(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writePlist(t, e.home)
	r := &fakeRunner{scripts: map[string]fakeResult{"launchctl print": {"", errors.New("exit 113")}}}
	changed, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{Answers: []string{"n"}}, testNow)
	require.NoError(t, err)
	assert.Empty(t, changed)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "is not running; Slack messages won't reach this Codex home")
	assert.Equal(t, []string{"launchctl print " + e.target}, r.calls)
}

func TestEnsureAppServerMismatchLeavesPlist(t *testing.T) {
	e := newAsEnv(t, ".codex-test")
	e.writePlist(t, "/somewhere/else")
	before, _ := os.ReadFile(e.plist)
	r := &fakeRunner{scripts: map[string]fakeResult{"launchctl print": {"state = running\n", nil}}}
	_, notes, err := ensureAppServer(e.home, e.user, r, &Scripted{}, testNow)
	require.NoError(t, err)
	require.NotEmpty(t, notes)
	all := strings.Join(notes, "\n")
	assert.Contains(t, all, "app-server plist "+e.plist+": CODEX_HOME is \"/somewhere/else\", expected")
	assert.Contains(t, all, "(left unchanged)")
	after, _ := os.ReadFile(e.plist)
	assert.Equal(t, before, after)
}

func TestVerifyAppServerPlistDefaultHomeWithoutCodexHome(t *testing.T) {
	e := newAsEnv(t, ".codex")
	e.writeSupervisor(t)
	plist := strings.Replace(string(renderAppServerPlist(e.home, e.user, e.supervisor, "u")), "<key>CODEX_HOME</key>\n\t\t<string>"+e.home+"</string>\n", "", 1)
	assert.Empty(t, verifyAppServerPlist(plist, e.home, e.user, e.label))
	assert.NotEmpty(t, verifyAppServerPlist(plist, filepath.Join(e.user, ".codex-x"), e.user, appServerLabel(filepath.Join(e.user, ".codex-x"))))
	assert.NotEmpty(t, verifyAppServerPlist(plist, e.home, e.user, "other.label"))
	assert.NotEmpty(t, verifyAppServerPlist(strings.ReplaceAll(plist, e.supervisor, "/no/such"), e.home, e.user, e.label))
}
