BIN     := ballast
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.1-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test fmt vet clean cross

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/ballast

run: build
	./bin/$(BIN) --listen 127.0.0.1:11435

test:
	go test ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

clean:
	rm -rf bin dist

# Binarios para todas las plataformas objetivo.
cross:
	mkdir -p dist
	for os in linux darwin windows; do \
		for arch in amd64 arm64; do \
			ext=""; [ $$os = windows ] && ext=".exe"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
				go build -trimpath -ldflags "$(LDFLAGS)" \
				-o dist/$(BIN)_$(VERSION)_$${os}_$${arch}$${ext} ./cmd/ballast; \
		done; \
	done
