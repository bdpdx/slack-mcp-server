package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartScriptName(t *testing.T) {
	assert.Equal(t, "start-codex", startScriptName("/u/.codex"))
	assert.Equal(t, "start-codex-test", startScriptName("/u/.codex-test"))
	assert.Equal(t, "start-agents", startScriptName("/u/agents"))
}

// runStart writes the script for home into a temp bin dir with a fake codex
// that prints its working directory, CODEX_HOME and arguments, then runs it
// under /bin/bash (macOS's bash 3.2 in production).
func runStart(t *testing.T, projectRoot string, args ...string) (string, error) {
	return runStartIn(t, "", projectRoot, args...)
}

func runStartIn(t *testing.T, cwd, projectRoot string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	fake := "#!/bin/bash\necho \"cwd=$(pwd -P) home=$CODEX_HOME root=$PROJECT_ROOT\"\nfor a in \"$@\"; do echo \"arg=$a\"; done\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755))
	script := filepath.Join(bin, "start-codex-test")
	require.NoError(t, os.WriteFile(script, renderStartScript("/u/.codex-test", projectRoot), 0o755))
	cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestStartScriptRunsCodex(t *testing.T) {
	proj := t.TempDir()
	real, err := filepath.EvalSymlinks(proj)
	require.NoError(t, err)

	out, err := runStart(t, "''", "-m", "daybreak", "-p", proj, "resume", "--last", "--", "-m", "x")
	require.NoError(t, err, out)
	assert.Contains(t, out, "cwd="+real+" home=/u/.codex-test root="+proj)
	assert.Equal(t, "arg=--remote\narg=unix://\narg=-C\narg="+proj+"\narg=-m\narg=gpt-daybreak-blue-latest\narg=resume\narg=--last\narg=-m\narg=x\n",
		out[strings.Index(out, "arg="):], "daybreak alias; extra args and everything after -- pass through")

	out, err = runStart(t, "'"+proj+"'")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "arg=-m", "no model unless -m is given")
	assert.Contains(t, out, "arg="+proj, "PROJECT_ROOT from the file is used")

	other := t.TempDir()
	out, err = runStart(t, "'"+proj+"'", "--project-root", other, "--model", "gpt-x")
	require.NoError(t, err, out)
	assert.Contains(t, out, "root="+other, "-p overrides PROJECT_ROOT")
	assert.Contains(t, out, "arg=gpt-x", "other model names pass unchanged")
}

// With no -p and no PROJECT_ROOT, the current directory is the project.
func TestStartScriptDefaultsToCurrentDirectory(t *testing.T) {
	cwd := t.TempDir()
	real, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	out, err := runStartIn(t, cwd, "''")
	require.NoError(t, err, out)
	assert.Contains(t, out, "cwd="+real)
	assert.Contains(t, out, "arg=-C\narg="+real)
}

func TestStartScriptRejectsBadArguments(t *testing.T) {
	out, err := runStart(t, "''", "-p", "/nonexistent/dir")
	assert.Error(t, err)
	assert.Contains(t, out, "is not a directory")

	out, err = runStart(t, "''", "-m")
	assert.Error(t, err)
	assert.Contains(t, out, "-m needs a model name")
}

// Re-running setup rewrites the script but keeps a PROJECT_ROOT the user set.
func TestInstallStartScriptKeepsProjectRoot(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin", "slack-mcp-server")
	path, changed, err := installStartScript("/u/.codex-test", bin, testNow)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, filepath.Join(filepath.Dir(bin), "start-codex-test"), path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "export PROJECT_ROOT=''", `export PROJECT_ROOT="$HOME/code/my-project"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o755))
	_, changed, err = installStartScript("/u/.codex-test", bin, testNow)
	require.NoError(t, err)
	assert.False(t, changed, "nothing else to update")
	data, _ = os.ReadFile(path)
	assert.Contains(t, string(data), `export PROJECT_ROOT="$HOME/code/my-project"`)
}
