package folder

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errBadName = errors.New("invalid path")

// validName rejects anything that could escape the folder root or touch our
// own metadata. Names come from remote devices and are never trusted.
func validName(name string) bool {
	if name == "" || name == "." || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	if strings.ContainsAny(name, "\\\x00") {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if c == ".." || c == "" {
			return false
		}
		if onWindows && windowsNameProblem(c) {
			return false
		}
	}
	// Compared case-insensitively: ".P2PSYNC" is the same directory on Windows.
	return !strings.EqualFold(strings.Split(name, "/")[0], MetaDir)
}

// onWindows enables the Windows-only name rules. It is a variable so tests on any
// system can exercise them through windowsNameProblem.
var onWindows = runtime.GOOS == "windows"

// caseInsensitive is true where "A.txt" and "a.txt" are the same file.
var caseInsensitive = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// windowsNameProblem reports whether one path component cannot be a normal file
// on Windows: characters it forbids (':' also introduces an NTFS alternate data
// stream), a trailing dot or space (silently stripped, so it would alias another
// name), device names such as CON or NUL (with or without an extension), and
// over-long names.
func windowsNameProblem(c string) bool {
	if c == "" || len(c) > 255 || strings.ContainsAny(c, `<>:"|?*`) {
		return true
	}
	for _, r := range c {
		if r < 0x20 {
			return true
		}
	}
	if last := c[len(c)-1]; last == '.' || last == ' ' {
		return true
	}
	base := c
	if i := strings.IndexByte(c, '.'); i >= 0 {
		base = c[:i]
	}
	return windowsReserved[strings.ToUpper(strings.TrimRight(base, " "))]
}

// abs maps a validated name to an absolute path, refusing to traverse symlinks
// so that a link planted inside the folder cannot redirect writes elsewhere.
func (f *Folder) abs(name string) (string, error) {
	if !validName(name) {
		return "", errBadName
	}
	parts := strings.Split(name, "/")
	cur := f.root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if err != nil {
			break // does not exist yet; nothing below it can be a symlink
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s: refusing to follow symlink", cur)
		}
	}
	return filepath.Join(f.root, filepath.FromSlash(name)), nil
}

// conflictName builds the name of the copy that preserves a losing version:
// "report.docx" -> "report.sync-conflict-20260102-150405-ABCDEF.docx".
func conflictName(name, devShort string, t time.Time) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if len(devShort) > 6 {
		devShort = devShort[:6]
	}
	return fmt.Sprintf("%s.sync-conflict-%s-%s%s", base, t.Format("20060102-150405"), devShort, ext)
}

// uniqueConflictName is conflictName, made distinct from anything that exists:
// two conflicts on the same file within one second must not overwrite each other.
func uniqueConflictName(root, name, devShort string, t time.Time) string {
	cand := conflictName(name, devShort, t)
	for i := 2; i < 1000; i++ {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(cand))); os.IsNotExist(err) {
			return cand
		}
		ext := path.Ext(name)
		cand = fmt.Sprintf("%s.sync-conflict-%s-%s-%d%s", strings.TrimSuffix(name, ext), t.Format("20060102-150405"), devShort[:min(6, len(devShort))], i, ext)
	}
	return cand
}
