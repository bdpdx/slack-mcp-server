package agentchat

// A watch is pinned to the agent process that launched it (CS-42): the
// listener drops it the moment that process exits. A process is named by its
// PID and start time, so a PID the OS reuses for a new process never counts
// as the old one.

// procAlive reports whether process pid is running and, when start is not
// 0, started at start (procStart's value).
func procAlive(pid int, start int64) bool {
	if pid <= 0 {
		return false
	}
	s, ok := procStart(pid)
	return ok && (start == 0 || s == start)
}
