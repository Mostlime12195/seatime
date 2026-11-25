//go:build !darwin && !linux && !windows

package idle

func defaultSource() Source { return None{Reason: "no idle timer on this OS"} }
