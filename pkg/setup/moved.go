package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// hostArch picks which Codex releases run here. A variable for tests, like hostOS.
var hostArch = runtime.GOARCH

// pathStart is what may come before a path in a config file: not more of a
// path, unless that is the "n" of an escaped newline in a TOML or JSON string.
const pathStart = `(^|[^\w./-]|\\[nrt])`

// movedRoots returns the user directories, other than userHome, that text says
// hold home: /Users/alice for a home now at /home/alice/.codex-work that still
// mentions /Users/alice/.codex-work. A home outside userHome has no such match
// to go by, so it returns nothing.
func movedRoots(home, userHome, text string) []string {
	if filepath.Dir(home) != userHome {
		return nil
	}
	re := regexp.MustCompile(pathStart + `((?:/Users|/home)/[^/\s"'\\]+)/` +
		regexp.QuoteMeta(filepath.Base(home)) + `(?:[/"':\s\\]|$)`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if m[2] != userHome {
			found[m[2]] = true
		}
	}
	roots := make([]string, 0, len(found))
	for root := range found {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

// rewriteMovedPaths points the named files of a home copied from another
// machine or user at userHome: everything under the old user directory, such
// as project trust and sandbox roots, not only what setup itself wrote. The
// old directory is found by the home's own path in those files, so a home that
// was not copied is left alone. Each changed file is backed up beside itself.
func rewriteMovedPaths(home, userHome string, names []string, now time.Time) ([]string, error) {
	files := map[string]string{}
	roots := map[string]bool{}
	for _, name := range names {
		path := filepath.Join(home, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		files[path] = string(data)
		for _, root := range movedRoots(home, userHome, string(data)) {
			roots[root] = true
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	repl := "${1}" + strings.ReplaceAll(userHome, "$", "$$") + "/"
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var changed []string
	for _, path := range paths {
		text := files[path]
		for root := range roots {
			text = regexp.MustCompile(pathStart+regexp.QuoteMeta(root)+`/`).ReplaceAllString(text, repl)
		}
		perm := os.FileMode(0o600)
		if info, err := os.Stat(path); err == nil {
			perm = info.Mode().Perm()
		}
		wrote, err := replaceFile(path, []byte(text), perm, now)
		if err != nil {
			return changed, err
		}
		if wrote {
			changed = append(changed, path+": paths from where this home was copied now point to "+userHome)
		}
	}
	return changed, nil
}

// releaseFitsHost reports whether a Codex release, named like
// 0.166.0-aarch64-apple-darwin or 0.166.0-x86_64-unknown-linux-musl, runs on
// this machine. A name that doesn't say is taken to fit.
func releaseFitsHost(release string) bool {
	wantOS, otherOS := "linux", "darwin"
	if hostOS == "darwin" {
		wantOS, otherOS = otherOS, wantOS
	}
	wantArch, otherArch := "x86_64", "aarch64"
	if hostArch == "arm64" {
		wantArch, otherArch = otherArch, wantArch
	}
	foreign := strings.Contains(release, otherOS) && !strings.Contains(release, wantOS)
	return !foreign && !(strings.Contains(release, otherArch) && !strings.Contains(release, wantArch))
}

// daemonPackageBroken reports whether a home's app-server daemon package can't
// run here, as when the home was copied from another machine: current links
// to a path that isn't here or a release for another system, or there is no
// package although daemon logs say there was one. Codex won't install a
// package for a home in the last state (see repairDaemonPackage).
func daemonPackageBroken(home string) bool {
	current := filepath.Join(home, "packages", "app-server-daemon", "current")
	target, err := os.Readlink(current)
	if err != nil {
		if _, statErr := os.Lstat(current); statErr == nil {
			return false // not a link; not a layout we know
		}
		for _, name := range daemonInstalledMarkers {
			if exists(filepath.Join(home, "app-server-daemon", name)) {
				return true
			}
		}
		return false
	}
	if _, err := os.Stat(filepath.Join(current, "bin", "codex")); err != nil {
		return true
	}
	return !releaseFitsHost(filepath.Base(target))
}

// daemonInstalledMarkers are the files in <home>/app-server-daemon that make
// `codex app-server daemon start` take a home for an earlier install and
// refuse, with "daemon executable not found", to install a missing package.
var daemonInstalledMarkers = []string{"daemon.stderr.log", "daemon-updater.stderr.log", "daemon-updater.pid"}

// repairDaemonPackage clears a package that can't run here and what the daemon
// left with it, so the next `codex app-server daemon start` installs this
// machine's package. A daemon that answers is left alone. The package can
// always be installed again, so it is deleted; logs are kept as .previous, the
// name Codex rotates them to.
func repairDaemonPackage(home string, r Runner) ([]string, error) {
	if !daemonPackageBroken(home) {
		return nil, nil
	}
	codex, err := r.LookPath("codex")
	if err != nil {
		return nil, nil
	}
	if _, err := r.Run(codexHomeEnv(home), codex, "app-server", "daemon", "version"); err == nil {
		return nil, nil
	}
	if err := os.RemoveAll(filepath.Join(home, "packages", "app-server-daemon")); err != nil {
		return nil, err
	}
	state := filepath.Join(home, "app-server-daemon")
	for _, name := range []string{"daemon.stderr.log", "daemon-updater.stderr.log"} {
		if err := os.Rename(filepath.Join(state, name), filepath.Join(state, name+".previous")); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	for _, path := range []string{
		filepath.Join(state, "daemon.pid"),
		filepath.Join(state, "daemon-updater.pid"),
		filepath.Join(state, "daemon-updater.sock"),
		filepath.Join(home, "app-server-control", "app-server-control.sock"),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return []string{"removed the app-server daemon package in " + home + ", which can't run on this machine; Codex installs a new one when the app-server starts"}, nil
}

func codexHomeEnv(home string) []string { return []string{"CODEX_HOME=" + home} }

// Bounds on waiting for a started app-server. The first start of a home
// installs its daemon package. Variables so tests don't wait.
var (
	appServerWait = 90 * time.Second
	appServerPoll = time.Second
)

// appServerStarted waits for a service just started to answer on its socket,
// since the service manager reports success as soon as the supervisor runs. It
// returns a note, naming logFile in <home>/app-server-daemon, if it never does.
func appServerStarted(home, label, logFile string, r Runner) []string {
	codex, err := r.LookPath("codex")
	if err != nil {
		return nil
	}
	for deadline := time.Now().Add(appServerWait); ; {
		if _, err := r.Run(codexHomeEnv(home), codex, "app-server", "daemon", "version"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(appServerPoll)
	}
	return []string{fmt.Sprintf("app-server %s did not come up; Slack messages won't reach this Codex home until it does. See %s",
		label, filepath.Join(home, "app-server-daemon", logFile))}
}
