#!/bin/bash
# Builds the Go engine as an Android library (AAR, 64-bit ARM only) into app/libs/.
# Requires: Go (the version in go.mod), the Android SDK with an NDK (set ANDROID_HOME),
# and gomobile matching the x/mobile version in go.mod (see tools.go):
#   go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${ANDROID_HOME:?set ANDROID_HOME to your Android SDK}"
NDK=$(ls -d "$ANDROID_HOME"/ndk/* 2>/dev/null | sort -V | tail -1)
[ -n "$NDK" ] || { echo "install an NDK: sdkmanager 'ndk;27.2.12479018'"; exit 1; }
export ANDROID_NDK_HOME="$NDK" PATH="$(go env GOPATH)/bin:$PATH"
mkdir -p android/app/libs
# -checklinkname=0: a libp2p dependency (anet) reads network interfaces through a
#   Go-internal symbol to work around Android's restrictions.
# max-page-size=16384: phones with 16 KB memory pages (Android 15+) refuse to load
#   libraries aligned for 4 KB pages.
# -trimpath, -buildid=: the same sources give the same bytes, with no local paths in them.
gomobile bind -target=android/arm64 -androidapi 26 -trimpath \
  -ldflags="-checklinkname=0 -s -w -buildid= -extldflags=-Wl,-z,max-page-size=16384" \
  -o android/app/libs/p2psync.aar ./mobile
rm -f android/app/libs/p2psync-sources.jar
ls -lh android/app/libs/p2psync.aar
