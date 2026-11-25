//go:build !windows

package main

import "syscall"

func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 1 << 60
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func selfCPU() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(0, &ru)
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

func killPID(pid int) { syscall.Kill(pid, syscall.SIGTERM) }
