//go:build darwin || linux

package main

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// peakRSS includes native allocations and thread stacks, unlike Go heap statistics.
func peakRSS() uint64 {
	var usage unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	rss := uint64(usage.Maxrss)
	if runtime.GOOS == "linux" {
		rss *= 1024
	}
	return rss
}
