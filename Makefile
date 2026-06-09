BINARY := bin/bugbot
PKG    := ./cmd/bugbot

.PHONY: build test vet lint fmt clean

# build a single statically-linked binary (CGO disabled so modernc.org/sqlite
# stays pure-Go and the binary is portable).
build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -rf bin
