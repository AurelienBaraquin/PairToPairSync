#!/bin/bash
# Builds the Go engine as an Android library (AAR) into app/libs/.
# Requires: Go, the Android SDK with an NDK (set ANDROID_HOME), and gomobile:
#   go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${ANDROID_HOME:?set ANDROID_HOME to your Android SDK}"
NDK=$(ls -d "$ANDROID_HOME"/ndk/* 2>/dev/null | sort -V | tail -1)
[ -n "$NDK" ] || { echo "install an NDK: sdkmanager 'ndk;27.2.12479018'"; exit 1; }
export ANDROID_NDK_HOME="$NDK" PATH="$PATH:$(go env GOPATH)/bin"
mkdir -p android/app/libs
# -checklinkname=0: a libp2p dependency (anet) reads network interfaces through a
# Go-internal symbol to work around Android's restrictions.
gomobile bind -target=android/arm64,android/amd64 -androidapi 26 \
  -ldflags="-checklinkname=0 -s -w" -o android/app/libs/p2psync.aar ./mobile
ls -lh android/app/libs/p2psync.aar
