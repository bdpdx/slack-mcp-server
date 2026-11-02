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

func (e *unEnv) launchAgent(t *testing.T, home string) (plist, target string) {
	useLaunchd(t)
	label := appServerLabel(home)
	plist = filepath.Join(e.user, "Library", "LaunchAgents", label+".plist")
	require.NoError(t, os.MkdirAll(filepath.Dir(plist), 0o755))
	require.NoError(t, os.WriteFile(plist, []byte("<plist/>"), 0o644))
	return plist, "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
}

func TestUninstallLaunchAgentYes(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	plist, target := e.launchAgent(t, home)
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "y", "n", "n")
	e.run(t)
	assert.Contains(t, e.r.calls, "launchctl bootout "+target)
	assert.False(t, exists(plist))
	assert.Empty(t, baks(t, plist), "no backup in LaunchAgents")
	assert.Len(t, baks(t, filepath.Join(e.repo, ".install", "backups", filepath.Base(plist))), 1)
}

func TestUninstallLaunchAgentNoKeepsIt(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	plist, _ := e.launchAgent(t, home)
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "n", "n")
	e.run(t)
	assert.True(t, exists(plist))
	for _, c := range e.r.calls {
		assert.NotContains(t, c, "bootout")
	}
}

func TestUninstallLaunchAgentNotLoadedNoPlistIsSilent(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	useLaunchd(t)
	e.r.scripts = map[string]fakeResult{"launchctl print": {err: errors.New("not loaded")}}
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "n")
	res := e.run(t)
	assert.NotContains(t, e.p.Out.String(), "launch agent")
	assert.NotContains(t, strings.Join(res[0].Notes, "\n"), "launch agent")
}

func TestUninstallBootoutNotLoadedIgnored(t *testing.T) {
	e := newUnEnv(t)
	home := e.codexHome(t, ".codex-test")
	plist, _ := e.launchAgent(t, home)
	e.r.scripts = map[string]fakeResult{"launchctl bootout": {out: "Boot-out failed: 3: No such process", err: errors.New("exit 3")}}
	e.state(t, HomeState{Path: home, Type: TypeCodex})
	e.answer("y", "y", "n", "n")
	res := e.run(t)
	assert.False(t, exists(plist))
	assert.NotContains(t, strings.Join(res[0].Notes, "\n"), "FAILED")
}
