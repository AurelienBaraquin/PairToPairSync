//go:build !linux

package fsutil

// OnWindowsMount is only meaningful inside WSL.
func OnWindowsMount(string) bool { return false }
