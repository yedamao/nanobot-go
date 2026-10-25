//go:build unix && !darwin && !linux

package gatewayruntime

// processStartTime is unknown here; the identity is the process group.
func processStartTime(int) string { return "" }
