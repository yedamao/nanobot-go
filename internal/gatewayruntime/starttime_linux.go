package gatewayruntime

import (
	"os"
	"strconv"
	"strings"
)

// processStartTime is field 22 (starttime, in clock ticks since boot) of
// /proc/<pid>/stat, or empty.
func processStartTime(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return ""
	}
	// After "pid (comm) " the fields start at field 3.
	fields := strings.Fields(s[i+2:])
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}
