BINARY := bin/bugbot
PKG    := ./cmd/bugbot

# GRAMMAR_TAGS subsets the embedded tree-sitter grammars. The gotreesitter
# grammars package go:embeds ~206 compressed blobs (~20MB) by default, but the
# tree-sitter code-nav fallback tier only uses go/python/typescript/tsx/c/cpp.
# Under `grammar_subset`, the all-grammars registry+blobs are compiled out and
# only the grammar_subset_<lang> entries are linked, cutting the static binary
# by ~21MB. See internal/treesitter/grammars.go for the supported set.
GRAMMAR_TAGS := grammar_subset grammar_subset_go grammar_subset_python grammar_subset_typescript grammar_subset_tsx grammar_subset_c grammar_subset_cpp

.PHONY: build test vet lint fmt clean

# build a single statically-linked binary (CGO disabled so modernc.org/sqlite
# stays pure-Go and the binary is portable).
build:
	CGO_ENABLED=0 go build -trimpath -tags '$(GRAMMAR_TAGS)' -o $(BINARY) $(PKG)

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
