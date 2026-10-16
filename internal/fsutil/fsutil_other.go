//go:build !windows

package fsutil

import "os"

const permissionsAreNoop = false

func transient(error) bool { return false }

// Hide does nothing here: a leading dot already hides a file.
func Hide(string) error { return nil }

// Protect makes a directory private to its owner.
func Protect(path string) error { return os.Chmod(path, 0o700) }
