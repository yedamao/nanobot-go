package gatewayruntime

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartTime is pid's start time from sysctl kern.proc.pid, in
// seconds and microseconds, or empty.
func processStartTime(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	t := kp.Proc.P_starttime
	if t.Sec <= 0 {
		return ""
	}
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec)
}
