BINARY := zessen-go
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: test build lint release

test:
	go test -race ./...

lint:
	go vet ./...

build:
	mkdir -p bin
	go build -o bin/$(BINARY) ./cmd/zessen-go

release:
	mkdir -p bin
	for platform in $(PLATFORMS); do \
		OS=$${platform%/*}; \
		ARCH=$${platform#*/}; \
		o="bin/$(BINARY)-$${OS}-$${ARCH}"; \
		[ $$OS = windows ] && o="$$o.exe"; \
		GOOS=$$OS GOARCH=$$ARCH go build -o $$o ./cmd/zessen-go; \
	done
