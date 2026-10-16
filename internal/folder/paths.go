package folder

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
	}
	return strings.Split(name, "/")[0] != MetaDir
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
