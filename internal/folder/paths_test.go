package folder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidNameRejectsEscapes(t *testing.T) {
	for _, bad := range []string{"", ".", "/etc/passwd", "../x", "a/../../x", "a//b", "a/./b", `a\b`, "a\x00b", ".p2psync/tmp/x", "a/"} {
		if validName(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
	for _, good := range []string{"a", "a/b.txt", "dir/sub dir/é.txt", ".hidden", "x.p2psync"} {
		if !validName(good) {
			t.Errorf("%q must be accepted", good)
		}
	}
}

func TestAbsRefusesSymlinkedParents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	f := &Folder{root: root}
	if _, err := f.abs("link/evil.txt"); err == nil {
		t.Fatal("must not write through a symlinked directory")
	}
	if _, err := f.abs("plain/ok.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoreMatcher(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, IgnoreFile), []byte("# c\n*.tmp\nbuild/\n/docs/draft*\n"), 0o644)
	m := loadIgnore(root)
	cases := map[string]bool{
		"a.tmp": true, "x/y/a.tmp": true, "build/out": true, "src/build/out": true,
		"docs/draft1.md": true, "other/docs/draft1.md": false, "docs/final.md": false,
		"keep.txt": false, ".p2psync/tmp/x": true, ".p2pignore": true,
	}
	for p, want := range cases {
		if got := m.Ignored(p, false); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
	if m.Ignored("build", false) { // a *file* named build is not the ignored directory
		t.Error("dirOnly pattern must not match a file")
	}
	if !m.Ignored("build", true) {
		t.Error("dirOnly pattern must match a directory")
	}
}
