//go:build tools

// Build tools, recorded here so go.mod pins their versions (see android/scripts/build-aar.sh).
package tools

import (
	_ "golang.org/x/mobile/cmd/gobind"
	_ "golang.org/x/mobile/cmd/gomobile"
)
