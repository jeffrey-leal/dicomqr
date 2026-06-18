#!/usr/bin/env bash
set -euo pipefail
# Put the MinGW64 toolchain on PATH so the CGO build works from any shell, not
# only an MSYS2 MinGW64 terminal (gcc and its runtime DLLs live in mingw64/bin).
export PATH="/c/Program Files/Go/bin:/c/msys64/mingw64/bin:$PATH"

# Ensure OpenJPEG (for JPEG 2000 decoding) is installed. Only invokes pacman when
# the static library is missing, so normal builds need no network access.
if [ ! -f /c/msys64/mingw64/lib/libopenjp2.a ]; then
  echo "OpenJPEG not found; installing mingw-w64-x86_64-openjpeg2..."
  pacman -S --needed --noconfirm mingw-w64-x86_64-openjpeg2
fi

echo "Generating documentation (dicomqr-user-manual.md and .docx)..."
go run ./gendoc

echo "Building dicomqr.exe (release, with JPEG 2000 support)..."
# -tags openjpeg enables the OpenJPEG-backed JPEG 2000 decoder; libopenjp2 is
# statically linked (see the cgo LDFLAGS -l:libopenjp2.a in jpeg2000_openjpeg.go)
# so the exe stays self-contained.
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe GOAMD64=v3 \
  go build -tags openjpeg \
  -ldflags="-s -w -H windowsgui -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
echo "Built dicomqr.exe"
