//go:build !darwin && !linux

package agentchat

import (
	"os"
	"syscall"
)

// procStart reports only whether pid exists: the start time is unknown here.
func procStart(pid int) (int64, bool) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	return 0, p.Signal(syscall.Signal(0)) == nil
}

// watchExit is unsupported here; the listener's liveness sweep and the check
// before each delivery still catch an exited agent.
func watchExit(int, int64, func()) bool { return false }
