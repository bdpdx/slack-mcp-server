package agentchat

import (
	"errors"

	"golang.org/x/sys/unix"
)

const szomb = 5 // SZOMB in <sys/proc.h>: exited, not yet reaped

// procStart returns when process pid started (microseconds since the epoch),
// or false when there is no such running process.
func procStart(pid int) (int64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid || kp.Proc.P_stat == szomb {
		return 0, false
	}
	return kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec), true
}

// watchExit calls onExit once, from another goroutine, when process pid
// (started at start) exits, or at once if it already has. It waits on a
// kqueue NOTE_EXIT event: no polling. It reports whether it could watch.
func watchExit(pid int, start int64, onExit func()) bool {
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		unix.Close(kq)
		if errors.Is(err, unix.ESRCH) {
			go onExit() // already gone
			return true
		}
		return false
	}
	if !procAlive(pid, start) { // exited, or the PID was reused, before the watch was set
		unix.Close(kq)
		go onExit()
		return true
	}
	go func() {
		defer unix.Close(kq)
		events := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(kq, nil, events, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil || n > 0 {
				onExit()
				return
			}
		}
	}()
	return true
}
