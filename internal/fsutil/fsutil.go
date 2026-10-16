// Package fsutil holds the few file-system helpers that behave differently on
// Windows: replacing a file that another process (antivirus, indexer, backup)
// has open, hiding a folder, and keeping secrets private.
package fsutil

import (
	"os"
	"path/filepath"
	"time"
)

// Rename moves oldpath over newpath. On Windows a destination that another
// process holds open fails with "access denied" or a sharing violation, usually
// for a few milliseconds only; those errors are retried.
func Rename(oldpath, newpath string) error {
	var err error
	for attempt := 0; attempt < 12; attempt++ {
		if err = os.Rename(oldpath, newpath); err == nil || !transient(err) {
			return err
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	return err
}

// Remove deletes a file with the same retry policy as Rename.
func Remove(path string) error {
	var err error
	for attempt := 0; attempt < 12; attempt++ {
		if err = os.Remove(path); err == nil || os.IsNotExist(err) || !transient(err) {
			return err
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	return err
}

// WriteFileAtomic writes data so that readers see either the old or the new
// content, never a mix, and a crash never leaves a truncated file: it writes a
// uniquely named temporary file next to the target, flushes it to disk, then
// renames it over the target.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !permissionsAreNoop {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// ClearReadOnly removes the read-only attribute, which on Windows prevents a
// file from being replaced or deleted.
func ClearReadOnly(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o200 == 0 {
		_ = os.Chmod(path, fi.Mode().Perm()|0o200)
	}
}
