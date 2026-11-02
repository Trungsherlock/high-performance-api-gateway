//go:build !windows

package main

// Native CPU/RSS collection is currently implemented only on Windows.
func processUsage() (float64, uint64) { return 0, 0 }
