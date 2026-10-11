package agentchat

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// procStart returns when process pid started (clock ticks since boot), or
// false when there is no such running process.
func procStart(pid int) (int64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// Fields after the command name, which is in parentheses and may hold spaces.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(data[i+1:]))
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return 0, false
	}
	start, err := strconv.ParseInt(f[19], 10, 64) // field 22, starttime
	return start, err == nil
}

// watchExit calls onExit once, from another goroutine, when process pid
// (started at start) exits, or at once if it already has. It waits on a
// pidfd: no polling. It reports whether it could watch.
func watchExit(pid int, start int64, onExit func()) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			go onExit()
			return true
		}
		return false
	}
	if !procAlive(pid, start) {
		unix.Close(fd)
		go onExit()
		return true
	}
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			_, err := unix.Poll(fds, -1)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			onExit()
			return
		}
	}()
	return true
}
