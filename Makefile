.PHONY: build test dist
build:
	go build -o bin/p2psync ./cmd/p2psync
test:
	go test ./... -race -count=1
dist:
	@for t in linux/amd64 linux/arm64 windows/amd64 darwin/arm64 darwin/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/p2psync-$$os-$$arch$$ext ./cmd/p2psync; \
	done
	# Android needs a linker flag because a libp2p dependency (anet) works around Android's restricted network-interface API.
	GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w -checklinkname=0" -o dist/p2psync-android-arm64 ./cmd/p2psync
