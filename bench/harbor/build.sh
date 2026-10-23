#!/bin/bash
# Build the static linux/amd64 bugbot binary the Harbor agent uploads into
# task containers (same grammar subset as `make build`).
set -euo pipefail
cd "$(dirname "$0")/../.."
mkdir -p bench/harbor/bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -tags 'grammar_subset grammar_subset_go grammar_subset_python grammar_subset_typescript grammar_subset_tsx grammar_subset_c grammar_subset_cpp grammar_subset_c_sharp grammar_subset_rust' \
  -o bench/harbor/bin/bugbot .
echo "built bench/harbor/bin/bugbot"
