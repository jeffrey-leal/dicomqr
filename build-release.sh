#!/usr/bin/env bash
set -euo pipefail
export PATH="/c/Program Files/Go/bin:$PATH"
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe GOAMD64=v3 \
  go build -ldflags="-s -w -H windowsgui -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
echo "Built dicomqr.exe (release)"
