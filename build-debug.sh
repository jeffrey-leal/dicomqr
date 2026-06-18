#!/usr/bin/env bash
set -euo pipefail
# Put the MinGW64 toolchain on PATH so the CGO build works from any shell.
export PATH="/c/Program Files/Go/bin:/c/msys64/mingw64/bin:$PATH"
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe \
  go build -tags openjpeg -ldflags="-X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
echo "Built dicomqr.exe (debug)"
