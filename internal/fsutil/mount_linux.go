//go:build linux

package fsutil

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// OnWindowsMount reports whether path lives on a Windows drive seen from WSL
// (/mnt/c and friends). Such a mount does not deliver change notifications for what
// Windows programs write, so the folder has to be re-scanned more often, and file
// names follow Windows rules.
func OnWindowsMount(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	best, bestType := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, v := range fields {
			if v == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) || len(fields) < 5 {
			continue
		}
		mp := strings.ReplaceAll(fields[4], `\040`, " ")
		if (abs == mp || strings.HasPrefix(abs, strings.TrimSuffix(mp, "/")+"/")) && len(mp) >= len(best) {
			best, bestType = mp, fields[sep+1]
		}
	}
	return bestType == "drvfs" || (bestType == "9p" && strings.HasPrefix(best, "/mnt/"))
}
