package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var movedNow = time.Date(2026, 11, 1, 22, 0, 0, 0, time.UTC)

func TestMovedRoots(t *testing.T) {
	const home, user = "/home/bob/.codex-r1", "/home/bob"
	assert.Equal(t, []string{"/Users/bob"}, movedRoots(home, user, `[hooks.state."/Users/bob/.codex-r1/hooks.json:stop:0:0"]`))
	assert.Equal(t, []string{"/Users/bob"}, movedRoots(home, user, `args = ["/Users/bob/.codex-r1"]`))
	assert.Equal(t, []string{"/home/alice"}, movedRoots(home, user, `x = "/home/alice/.codex-r1/y"`))
	assert.Equal(t, []string{"/home/bob"}, movedRoots("/Users/bob/.codex-r1", "/Users/bob", `x = "/home/bob/.codex-r1/y"`))
	assert.Empty(t, movedRoots(home, user, `x = "/home/bob/.codex-r1/y"`), "already here")
	assert.Empty(t, movedRoots(home, user, `x = "/Users/bob/code/project"`), "no sign this is the home's own old path")
	assert.Empty(t, movedRoots(home, user, `x = "/Users/bob/.codex-r10/y"`), "another home")
	assert.Empty(t, movedRoots(home, user, `x = "/mnt/Users/bob/.codex-r1/y"`), "deeper path")
	assert.Empty(t, movedRoots("/data/.codex-r1", user, `x = "/Users/bob/.codex-r1/y"`), "home outside the user directory")
}

func TestRewriteMovedPaths(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	cfg := filepath.Join(e.home, "config.toml")
	require.NoError(t, os.WriteFile(cfg, []byte(`[projects."/Users/bob/code/repo"]
trust_level = "trusted"
[hooks.state."/Users/bob/.codex-r1/hooks.json:stop:0:0"]
writable_roots = ["/Users/bob/code/state"]
extra_policy = "line\n/Users/bob/code/other"
other = "/mnt/Users/bob/keep and /Users/bobby/keep"
`), 0o640))
	hooks := filepath.Join(e.home, "hooks.json")
	require.NoError(t, os.WriteFile(hooks, []byte(`{"a":"/Users/bob/bin/tool"}`), 0o600))

	changed, err := rewriteMovedPaths(e.home, e.user, []string{"config.toml", "hooks.json", "rules/default.rules"}, movedNow)
	require.NoError(t, err)
	assert.Len(t, changed, 2)

	got, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Equal(t, `[projects."`+e.user+`/code/repo"]
trust_level = "trusted"
[hooks.state."`+e.user+`/.codex-r1/hooks.json:stop:0:0"]
writable_roots = ["`+e.user+`/code/state"]
extra_policy = "line\n`+e.user+`/code/other"
other = "/mnt/Users/bob/keep and /Users/bobby/keep"
`, string(got))
	info, err := os.Stat(cfg)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	got, err = os.ReadFile(hooks)
	require.NoError(t, err)
	assert.Equal(t, `{"a":"`+e.user+`/bin/tool"}`, string(got), "a file with no home path follows the others")

	backups, err := filepath.Glob(cfg + ".bak*")
	require.NoError(t, err)
	assert.Len(t, backups, 1)

	changed, err = rewriteMovedPaths(e.home, e.user, []string{"config.toml", "hooks.json"}, movedNow)
	require.NoError(t, err)
	assert.Empty(t, changed, "running it again changes nothing")
}

func TestRewriteMovedPathsLeavesOtherHomesAlone(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	cfg := filepath.Join(e.home, "config.toml")
	const text = `writable_roots = ["/Users/bob/code/state"]` + "\n"
	require.NoError(t, os.WriteFile(cfg, []byte(text), 0o600))
	changed, err := rewriteMovedPaths(e.home, e.user, []string{"config.toml"}, movedNow)
	require.NoError(t, err)
	assert.Empty(t, changed)
	got, _ := os.ReadFile(cfg)
	assert.Equal(t, text, string(got))
}

func TestReleaseFitsHost(t *testing.T) {
	defer func(o, a string) { hostOS, hostArch = o, a }(hostOS, hostArch)
	cases := []struct {
		os, arch, release string
		want              bool
	}{
		{"linux", "arm64", "0.166.0-aarch64-unknown-linux-musl", true},
		{"linux", "arm64", "0.166.0-aarch64-apple-darwin", false},
		{"linux", "arm64", "0.166.0-x86_64-unknown-linux-musl", false},
		{"linux", "amd64", "0.166.0-x86_64-unknown-linux-musl", true},
		{"darwin", "arm64", "0.166.0-aarch64-apple-darwin", true},
		{"darwin", "arm64", "0.166.0-aarch64-unknown-linux-musl", false},
		{"darwin", "amd64", "0.166.0-aarch64-apple-darwin", false},
		{"linux", "arm64", "custom", true},
	}
	for _, c := range cases {
		hostOS, hostArch = c.os, c.arch
		assert.Equal(t, c.want, releaseFitsHost(c.release), "%s/%s %s", c.os, c.arch, c.release)
	}
}

// copiedHome gives e.home the daemon state of a home copied from a Mac: the
// sockets, pid and logs, and a current link to target with a darwin release.
func copiedHome(t *testing.T, e asEnv, target string) {
	state := filepath.Join(e.home, "app-server-daemon")
	control := filepath.Join(e.home, "app-server-control")
	pkg := filepath.Join(e.home, "packages", "app-server-daemon")
	bin := filepath.Join(pkg, "releases", "0.166.0-aarch64-apple-darwin", "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("mac"), 0o755))
	require.NoError(t, os.MkdirAll(state, 0o700))
	require.NoError(t, os.MkdirAll(control, 0o700))
	require.NoError(t, os.Symlink(target, filepath.Join(pkg, "current")))
	for _, name := range []string{"daemon.stderr.log", "daemon-updater.stderr.log", "daemon-updater.pid", "daemon.pid", "systemd.stderr.log"} {
		require.NoError(t, os.WriteFile(filepath.Join(state, name), []byte(name), 0o644))
	}
	require.NoError(t, os.Symlink("/private/tmp/gone", filepath.Join(state, "daemon-updater.sock")))
	require.NoError(t, os.Symlink("/private/tmp/gone", filepath.Join(control, "app-server-control.sock")))
	require.NoError(t, os.WriteFile(filepath.Join(control, "app-server-startup.lock"), nil, 0o644))
}

func daemonDown() *fakeRunner {
	return &fakeRunner{scripts: map[string]fakeResult{"/usr/local/bin/codex app-server daemon version": {"", errors.New("exit 1")}}}
}

func assertRepaired(t *testing.T, e asEnv) {
	t.Helper()
	state := filepath.Join(e.home, "app-server-daemon")
	assert.NoDirExists(t, filepath.Join(e.home, "packages", "app-server-daemon"))
	for _, name := range daemonInstalledMarkers {
		assert.NoFileExists(t, filepath.Join(state, name), "blocks a new install")
	}
	assert.NoFileExists(t, filepath.Join(state, "daemon.pid"))
	got, err := os.ReadFile(filepath.Join(state, "daemon.stderr.log.previous"))
	require.NoError(t, err)
	assert.Equal(t, "daemon.stderr.log", string(got))
	assert.FileExists(t, filepath.Join(state, "systemd.stderr.log"))
	_, err = os.Lstat(filepath.Join(state, "daemon-updater.sock"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Lstat(filepath.Join(e.home, "app-server-control", "app-server-control.sock"))
	assert.True(t, os.IsNotExist(err))
	assert.FileExists(t, filepath.Join(e.home, "app-server-control", "app-server-startup.lock"))
}

func TestRepairDaemonPackageDeadLink(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	copiedHome(t, e, "/Users/bob/.codex-r1/packages/app-server-daemon/releases/0.166.0-aarch64-apple-darwin")
	changed, err := repairDaemonPackage(e.home, daemonDown())
	require.NoError(t, err)
	assert.Len(t, changed, 1)
	assertRepaired(t, e)
}

func TestRepairDaemonPackageOtherSystem(t *testing.T) {
	defer func(o string) { hostOS = o }(hostOS)
	hostOS = "linux"
	e := newAsEnv(t, ".codex-r1")
	// The link resolves, as it would after copying a home to the same path.
	copiedHome(t, e, filepath.Join(e.home, "packages", "app-server-daemon", "releases", "0.166.0-aarch64-apple-darwin"))
	changed, err := repairDaemonPackage(e.home, daemonDown())
	require.NoError(t, err)
	assert.Len(t, changed, 1)
	assertRepaired(t, e)
}

func TestRepairDaemonPackageMissingAfterInstall(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	copiedHome(t, e, "/gone")
	require.NoError(t, os.RemoveAll(filepath.Join(e.home, "packages", "app-server-daemon")))
	changed, err := repairDaemonPackage(e.home, daemonDown())
	require.NoError(t, err)
	assert.Len(t, changed, 1)
	assertRepaired(t, e)
}

func TestRepairDaemonPackageLeavesAnswerAlone(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	copiedHome(t, e, "/gone")
	changed, err := repairDaemonPackage(e.home, &fakeRunner{}) // daemon version succeeds
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.DirExists(t, filepath.Join(e.home, "packages", "app-server-daemon"))
}

func TestRepairDaemonPackageLeavesHealthyAlone(t *testing.T) {
	defer func(o, a string) { hostOS, hostArch = o, a }(hostOS, hostArch)
	hostOS, hostArch = "darwin", "arm64"
	e := newAsEnv(t, ".codex-r1")
	copiedHome(t, e, filepath.Join(e.home, "packages", "app-server-daemon", "releases", "0.166.0-aarch64-apple-darwin"))
	r := daemonDown()
	changed, err := repairDaemonPackage(e.home, r)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Empty(t, r.calls, "a good package needs no question to the daemon")
	assert.DirExists(t, filepath.Join(e.home, "packages", "app-server-daemon"))
}

func TestRepairDaemonPackageFreshHome(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	require.NoError(t, os.MkdirAll(filepath.Join(e.home, "app-server-daemon"), 0o700))
	r := daemonDown()
	changed, err := repairDaemonPackage(e.home, r)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Empty(t, r.calls)
}

func TestRepairDaemonPackageWithoutCodex(t *testing.T) {
	e := newAsEnv(t, ".codex-r1")
	copiedHome(t, e, "/gone")
	r := daemonDown()
	r.missing = map[string]bool{"codex": true}
	changed, err := repairDaemonPackage(e.home, r)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.DirExists(t, filepath.Join(e.home, "packages", "app-server-daemon"), "nothing could install another")
}

func TestEnsureAppServerNotesServiceThatNeverAnswers(t *testing.T) {
	defer func(w, p time.Duration) { appServerWait, appServerPoll = w, p }(appServerWait, appServerPoll)
	appServerWait, appServerPoll = 0, 0
	defer func(o string) { hostOS = o }(hostOS)
	hostOS = "linux"
	e := newAsEnv(t, ".codex-r1")
	changed, notes, err := ensureAppServer(e.home, e.user, daemonDown(), &Scripted{})
	require.NoError(t, err)
	assert.Contains(t, changed, "app-server service "+e.label+" created and started")
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "did not come up")
	assert.Contains(t, notes[0], filepath.Join(e.home, "app-server-daemon", "systemd.stderr.log"))
}

func TestEnsureAppServerRepairsBeforeStarting(t *testing.T) {
	defer func(o string) { hostOS = o }(hostOS)
	hostOS = "linux"
	e := newAsEnv(t, ".codex-r1")
	copiedHome(t, e, "/Users/bob/.codex-r1/packages/app-server-daemon/current-release")
	r := &fakeRunner{scripts: map[string]fakeResult{"/usr/local/bin/codex app-server daemon version": {"", errors.New("exit 1")}}}
	defer func(w, p time.Duration) { appServerWait, appServerPoll = w, p }(appServerWait, appServerPoll)
	appServerWait, appServerPoll = 0, 0
	changed, _, err := ensureAppServer(e.home, e.user, r, &Scripted{})
	require.NoError(t, err)
	assertRepaired(t, e)
	require.NotEmpty(t, changed)
	assert.Contains(t, changed[0], "removed the app-server daemon package")
}

func TestInstallClaudeRewritesMovedSettings(t *testing.T) {
	e := newAsEnv(t, ".claude-r")
	settings := filepath.Join(e.home, "settings.json")
	require.NoError(t, os.WriteFile(settings, []byte(`{"statusLine":{"command":"/Users/bob/.claude-r/status.sh"}}`), 0o600))
	res, err := InstallClaude(e.home, e.user, "/usr/local/bin/slack-mcp-server", &fakeRunner{missing: map[string]bool{"claude": true}}, movedNow)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Changed)
	got, err := os.ReadFile(settings)
	require.NoError(t, err)
	assert.Contains(t, string(got), e.user+"/.claude-r/status.sh")
	assert.NotContains(t, string(got), "/Users/bob")
}
