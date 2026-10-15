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
	assert.Equal(t, "start-claude-b", startScriptName("/u/.claude-b"))
	assert.Equal(t, "start-codex-test", startScriptName("/u/.codex-test"))
	assert.Equal(t, "start-agents", startScriptName("/u/agents"))
}

// runStart writes the script for home into a temp bin dir with a fake codex
// that prints its working directory, CODEX_HOME and arguments, then runs it
// under /bin/bash (macOS's bash 3.2 in production).
func runStart(t *testing.T, repoRoot string, args ...string) (string, error) {
	return runStartIn(t, "", repoRoot, args...)
}

func runStartIn(t *testing.T, cwd, repoRoot string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	fake := "#!/bin/bash\necho \"cwd=$(pwd -P) home=$CODEX_HOME root=$REPO_ROOT\"\nfor a in \"$@\"; do echo \"arg=$a\"; done\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755))
	script := filepath.Join(bin, "start-codex-test")
	require.NoError(t, os.WriteFile(script, renderStartScript(TypeCodex, "/u/.codex-test", "/u", repoRoot), 0o755))
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

	out, err := runStart(t, "''", "-m", "daybreak", "-r", proj, "resume", "--last", "--", "-m", "x")
	require.NoError(t, err, out)
	assert.Contains(t, out, "cwd="+real+" home=/u/.codex-test root="+proj)
	assert.Equal(t, "arg=--remote\narg=unix://\narg=-C\narg="+proj+"\narg=-m\narg=gpt-daybreak-blue-latest\narg=resume\narg=--last\narg=-m\narg=x\n",
		out[strings.Index(out, "arg="):], "daybreak alias; extra args and everything after -- pass through")

	out, err = runStart(t, "'"+proj+"'")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "arg=-m", "no model unless -m is given")
	assert.Contains(t, out, "arg="+proj, "REPO_ROOT from the file is used")

	other := t.TempDir()
	out, err = runStart(t, "'"+proj+"'", "--repo-root", other, "--model", "gpt-x")
	require.NoError(t, err, out)
	assert.Contains(t, out, "root="+other, "-r overrides REPO_ROOT")
	assert.Contains(t, out, "arg=gpt-x", "other model names pass unchanged")
}

// With no -p and no REPO_ROOT, the current directory is the project.
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
	out, err := runStart(t, "''", "-r", "/nonexistent/dir")
	assert.Error(t, err)
	assert.Contains(t, out, "is not a directory")

	out, err = runStart(t, "''", "-m")
	assert.Error(t, err)
	assert.Contains(t, out, "-m needs a model name")
}

// Re-running setup rewrites the script but keeps a REPO_ROOT the user set.
func TestInstallStartScriptKeepsRepoRoot(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin", "slack-mcp-server")
	path, changed, err := installStartScript(TypeCodex, "/u/.codex-test", "/u", bin, nil, testNow)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, filepath.Join(filepath.Dir(bin), "start-codex-test"), path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "export REPO_ROOT=''", `export REPO_ROOT="$HOME/code/my-project"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o755))
	_, changed, err = installStartScript(TypeCodex, "/u/.codex-test", "/u", bin, nil, testNow)
	require.NoError(t, err)
	assert.False(t, changed, "nothing else to update")
	data, _ = os.ReadFile(path)
	assert.Contains(t, string(data), `export REPO_ROOT="$HOME/code/my-project"`)
}

// runClaudeStart runs the Claude Code script for /u/.claude-b in cwd with a
// fake claude that prints its working directory, CLAUDE_CONFIG_DIR and
// arguments.
func runClaudeStart(t *testing.T, cwd, repoRoot string, args ...string) (string, error) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	fake := "#!/bin/bash\necho \"cwd=$(pwd -P) config=$CLAUDE_CONFIG_DIR root=$REPO_ROOT\"\nfor a in \"$@\"; do echo \"arg=$a\"; done\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o755))
	script := filepath.Join(bin, "start-claude-b")
	require.NoError(t, os.WriteFile(script, renderStartScript(TypeClaude, "/u/.claude-b", "/u", repoRoot), 0o755))
	cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestClaudeStartScriptRunsClaude(t *testing.T) {
	proj := t.TempDir()
	real, err := filepath.EvalSymlinks(proj)
	require.NoError(t, err)

	out, err := runClaudeStart(t, "", "''", "-r", proj, "-p", "hi", "--", "-r")
	require.NoError(t, err, out)
	assert.Contains(t, out, "cwd="+real+" config=/u/.claude-b root="+proj)
	assert.Equal(t, "arg=-p\narg=hi\narg=-r\n", out[strings.Index(out, "arg="):],
		"claude's own -p passes through; after --, so does -r; nothing else is added")

	other := t.TempDir()
	out, err = runClaudeStart(t, "", "'"+proj+"'", "--repo-root", other)
	require.NoError(t, err, out)
	assert.Contains(t, out, "root="+other, "-r overrides REPO_ROOT")
	assert.NotContains(t, out, "arg=", "no arguments of its own")

	cwd := t.TempDir()
	realCwd, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	out, err = runClaudeStart(t, cwd, "''")
	require.NoError(t, err, out)
	assert.Contains(t, out, "cwd="+realCwd, "no -p and no REPO_ROOT: the current directory")

	out, err = runClaudeStart(t, "", "''", "-r", "/nonexistent/dir")
	assert.Error(t, err)
	assert.Contains(t, out, "is not a directory")
}

func TestInstallClaudeStartScriptKeepsRepoRoot(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin", "slack-mcp-server")
	path, _, err := installStartScript(TypeClaude, "/u/.claude-b", "/u", bin, nil, testNow)
	require.NoError(t, err)
	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "export CLAUDE_CONFIG_DIR='/u/.claude-b'")
	assert.NotContains(t, string(data), "codex")
	edited := strings.Replace(string(data), "export REPO_ROOT=''", `export REPO_ROOT=~/code/x`, 1)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o755))
	_, changed, err := installStartScript(TypeClaude, "/u/.claude-b", "/u", bin, nil, testNow)
	require.NoError(t, err)
	assert.False(t, changed)
}

// The default home runs claude without CLAUDE_CONFIG_DIR, even one inherited.
func TestClaudeStartScriptDefaultHomeUnsetsConfigDir(t *testing.T) {
	script := renderStartScript(TypeClaude, "/u/.claude", "/u", "''")
	assert.Contains(t, string(script), "unset CLAUDE_CONFIG_DIR")
	assert.NotContains(t, string(script), "export CLAUDE_CONFIG_DIR")

	bin := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	fake := "#!/bin/bash\necho \"config=${CLAUDE_CONFIG_DIR-unset}\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o755))
	path := filepath.Join(bin, "start-claude")
	require.NoError(t, os.WriteFile(path, script, 0o755))
	cmd := exec.Command("/bin/bash", path, "-r", t.TempDir())
	cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "CLAUDE_CONFIG_DIR=/inherited")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, "config=unset\n", string(out))
}

func TestDisplayRepoRoot(t *testing.T) {
	assert.Equal(t, "", displayRepoRoot("''"))
	assert.Equal(t, "", displayRepoRoot(`""`))
	assert.Equal(t, "/a b/c", displayRepoRoot("'/a b/c'"))
	assert.Equal(t, `"$HOME/x"`, displayRepoRoot(`"$HOME/x"`), "anything else is shown as written")
	assert.Equal(t, `'it'\''s'`, displayRepoRoot(`'it'\''s'`))
}

// A script written when the variable was PROJECT_ROOT keeps its value as
// REPO_ROOT.
func TestInstallStartScriptCarriesOverProjectRoot(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin", "slack-mcp-server")
	path := startScriptPath(bin, "/u/.codex-test")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/bash\nexport PROJECT_ROOT='/old/repo'\n"), 0o755))
	raw, ok := currentRepoRoot(path)
	assert.True(t, ok)
	assert.Equal(t, "'/old/repo'", raw)
	_, _, err := installStartScript(TypeCodex, "/u/.codex-test", "/u", bin, nil, testNow)
	require.NoError(t, err)
	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "export REPO_ROOT='/old/repo'\n")
	assert.NotContains(t, string(data), "PROJECT_ROOT")
}
