#!/usr/bin/env bash
set -euo pipefail
export PATH="/c/Program Files/Go/bin:$PATH"
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe \
  go build -ldflags="-X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
echo "Built dicomqr.exe (debug)"
