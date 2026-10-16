package config

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// canon makes a path comparable: absolute, cleaned, symlinks resolved when the
// path exists, and lower-cased where the file system ignores case.
func canon(p string) string {
	p = filepath.Clean(p)
	// Resolve what exists (symbolic links, and on Windows 8.3 short names such as
	// RUNNER~1) and keep the rest as written: the private directory may not exist yet
	// while the folder around it does, and both must be compared in the same spelling.
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			p = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

func within(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}

// CheckPaths refuses layouts that would make the program synchronise its own
// private data or two folders into each other: a folder equal to, inside or
// around another one, or around the directory that holds the device key, the
// folder secrets and the indexes (home). Syncing home would hand the private
// key and the secrets to every peer, and let a peer overwrite them.
func (c *Config) CheckPaths(home string) error {
	h := canon(home)
	for i := range c.Folders {
		a := canon(c.Folders[i].Path)
		if within(h, a) || within(a, h) {
			return fmt.Errorf("folder %q (%s) overlaps the program's private data directory (%s)", c.Folders[i].ID, c.Folders[i].Path, home)
		}
		for j := i + 1; j < len(c.Folders); j++ {
			b := canon(c.Folders[j].Path)
			if within(a, b) || within(b, a) {
				return fmt.Errorf("folders %q (%s) and %q (%s) overlap: a folder cannot be equal to, inside or around another",
					c.Folders[i].ID, c.Folders[i].Path, c.Folders[j].ID, c.Folders[j].Path)
			}
		}
	}
	return nil
}
