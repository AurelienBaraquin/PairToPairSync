package config

import (
	"regexp"
	"strings"
	"unicode"
)

// Everything that arrives from another device (folder IDs, names, labels) is
// untrusted: it ends up in file names, in the configuration and in a web page.
// These rules keep it boring.

var folderIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidFolderID accepts short identifiers that are safe as part of a file name:
// no separators, no "..", no spaces.
func ValidFolderID(id string) bool {
	return folderIDRe.MatchString(id) && !strings.Contains(id, "..")
}

// SlugID turns an arbitrary directory name into a valid folder ID.
func SlugID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)), r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), ".-_")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" || !ValidFolderID(s) {
		return "folder"
	}
	return s
}

// Clean reduces a display name or label to letters, digits, spaces and a few
// punctuation marks, at most 64 characters. No markup characters survive.
func Clean(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= 64 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune("._-()'", r) {
			b.WriteRune(r)
			n++
		}
	}
	return strings.TrimSpace(b.String())
}
