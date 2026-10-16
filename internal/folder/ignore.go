package folder

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MetaDir is the hidden directory at the folder root holding the folder marker
// and temporary downloads. It is never synchronised.
const MetaDir = ".p2psync"

// IgnoreFile lists patterns (one per line, # for comments) that are not synchronised.
const IgnoreFile = ".p2pignore"

type pattern struct {
	glob     string
	anchored bool // contains a slash: match against the full relative path
	dirOnly  bool
}

// Matcher implements a small gitignore-like syntax: "*.tmp", "build/", "/docs/draft*".
type Matcher struct{ pats []pattern }

func loadIgnore(root string) *Matcher {
	m := &Matcher{}
	f, err := os.Open(filepath.Join(root, IgnoreFile))
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := pattern{}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.Contains(line, "/") {
			p.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		p.glob = line
		m.pats = append(m.pats, p)
	}
	return m
}

// Ignored reports whether rel (slash separated) must be skipped. A path is
// ignored when it or any of its parent directories matches.
func (m *Matcher) Ignored(rel string, isDir bool) bool {
	parts := strings.Split(rel, "/")
	if strings.EqualFold(parts[0], MetaDir) || rel == IgnoreFile {
		return true
	}
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		prefixIsDir := i < len(parts)-1 || isDir
		for _, p := range m.pats {
			if p.dirOnly && !prefixIsDir {
				continue
			}
			target := parts[i]
			if p.anchored {
				target = prefix
			}
			if ok, _ := path.Match(p.glob, target); ok {
				return true
			}
		}
	}
	return false
}
