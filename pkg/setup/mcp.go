package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner runs the agents' CLIs (faked in tests). Each env entry is either
// NAME=value (set, replacing any inherited value) or NAME (unset).
type Runner interface {
	LookPath(name string) (string, error)
	Run(env []string, name string, args ...string) (string, error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (ExecRunner) Run(env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = commandEnv(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commandEnv applies env (NAME=value sets, NAME unsets) to base, dropping
// every inherited value of a named variable.
func commandEnv(base, env []string) []string {
	named := map[string]bool{}
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		named[k] = true
	}
	var out []string
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !named[k] {
			out = append(out, kv)
		}
	}
	for _, e := range env {
		if strings.Contains(e, "=") {
			out = append(out, e)
		}
	}
	return out
}

// RegisterMCP (re)registers the slack MCP server for one home. When the
// agent's CLI is not installed it returns the command to run later. The
// default Claude home (~/.claude) runs claude without CLAUDE_CONFIG_DIR; any
// other home sets it, and Codex always gets CODEX_HOME.
func RegisterMCP(r Runner, kind, home, userHome, bin string) (string, error) {
	serve := []string{"--", bin, "--transport", "stdio", "--env-file", EnvPath(home)}
	var name string
	var env, rm, add []string
	switch kind {
	case TypeClaude:
		name, rm, add = "claude", []string{"mcp", "remove", "-s", "user", "slack"}, append([]string{"mcp", "add", "-s", "user", "slack"}, serve...)
		if filepath.Clean(home) == filepath.Join(userHome, ".claude") {
			env = []string{"CLAUDE_CONFIG_DIR"}
		} else {
			env = []string{"CLAUDE_CONFIG_DIR=" + home}
		}
	default:
		name, env = "codex", []string{"CODEX_HOME=" + home}
		rm, add = []string{"mcp", "remove", "slack"}, append([]string{"mcp", "add", "slack"}, serve...)
	}
	if _, err := r.LookPath(name); err != nil {
		quoted := make([]string, len(add))
		for i, a := range add {
			quoted[i] = shellQuote(a)
		}
		var prefix string
		if k, v, ok := strings.Cut(env[0], "="); ok {
			prefix = k + "=" + shellQuote(v) + " "
		} else {
			prefix = "env -u " + k + " "
		}
		return prefix + name + " " + strings.Join(quoted, " "), nil
	}
	_, _ = r.Run(env, name, rm...) // absent is fine
	if out, err := r.Run(env, name, add...); err != nil {
		return "", &cliError{name: name, out: out, err: err}
	}
	return "", nil
}

type cliError struct {
	name, out string
	err       error
}

func (e *cliError) Error() string {
	return e.name + " mcp add failed: " + e.err.Error() + ": " + strings.TrimSpace(e.out)
}
