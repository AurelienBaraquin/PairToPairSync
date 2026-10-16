# Supported targets: Windows x64, Ubuntu x64 (including WSL) and Android (arm64).
# The code also builds elsewhere, but nothing else is shipped or tested.
.PHONY: build test windows linux android check
GOFLAGS_REL = -trimpath -ldflags "-s -w -buildid="

build:
	go build -o bin/p2psync ./cmd/p2psync

test:
	go test ./... -race -count=1

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	go test ./... -race -count=1

# p2psync.exe: the command line. p2psyncd.exe: the same program without a console
# window, for the logon task (packaging/windows-autostart.ps1).
windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS_REL) -o dist/p2psync.exe ./cmd/p2psync
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid= -H=windowsgui" -o dist/p2psyncd.exe ./cmd/p2psync

# Ubuntu (also inside WSL): one static binary, see docs/WSL.md.
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS_REL) -o dist/p2psync-linux-amd64 ./cmd/p2psync

# Needs ANDROID_HOME (SDK + NDK) and JDK 17+; produces android/app/build/outputs/apk/.
android:
	./android/scripts/build-aar.sh
	cd android && ./gradlew testDebugUnitTest lintDebug assembleDebug
