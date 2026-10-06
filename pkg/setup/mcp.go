package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner runs the agents' CLIs (faked in tests).
type Runner interface {
	LookPath(name string) (string, error)
	Run(env []string, name string, args ...string) (string, error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (ExecRunner) Run(env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RegisterMCP (re)registers the slack MCP server for one home. When the
// agent's CLI is not installed it returns the command to run later.
func RegisterMCP(r Runner, kind, home, bin string) (string, error) {
	serve := []string{"--", bin, "--transport", "stdio", "--env-file", EnvPath(home)}
	var name string
	var env, rm, add []string
	switch kind {
	case TypeClaude:
		name, rm, add = "claude", []string{"mcp", "remove", "-s", "user", "slack"}, append([]string{"mcp", "add", "-s", "user", "slack"}, serve...)
		if filepath.Base(home) != ".claude" {
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
		prefix := ""
		if len(env) > 0 {
			prefix = shellQuote(env[0]) + " "
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
