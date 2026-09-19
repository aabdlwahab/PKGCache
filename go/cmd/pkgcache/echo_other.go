//go:build !linux && !darwin && !windows

package main

// withoutEcho reads as typed where there is no terminal interface this knows how to quiet.
func withoutEcho(_ int, read func() (string, error)) (string, error) { return read() }
