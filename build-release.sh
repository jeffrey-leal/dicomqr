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

# Ensure libjpeg-turbo (for JPEG Lossless decoding) is installed. Same policy.
if [ ! -f /c/msys64/mingw64/lib/libjpeg.a ]; then
  echo "libjpeg-turbo not found; installing mingw-w64-x86_64-libjpeg-turbo..."
  pacman -S --needed --noconfirm mingw-w64-x86_64-libjpeg-turbo
fi

echo "Generating documentation (dicomqr-user-manual.md and .docx)..."
go run ./gendoc

echo "Building dicomqr.exe (release, with JPEG 2000 and JPEG Lossless support)..."
# -tags openjpeg enables the OpenJPEG-backed JPEG 2000 decoder; jpeglossless
# enables the libjpeg-turbo-backed JPEG Lossless decoder. Both libraries are
# statically linked (cgo LDFLAGS -l:libopenjp2.a / -l:libjpeg.a in
# jpeg2000_openjpeg.go / jpeglossless_turbo.go) so the exe stays self-contained.
# -extldflags=-static makes GCC link its own runtime (libwinpthread, libgcc)
# statically too — without it the exe imports libwinpthread-1.dll, which only
# exists on machines with MSYS2 installed.
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe GOAMD64=v3 \
  go build -tags "openjpeg jpeglossless" \
  -ldflags="-s -w -H windowsgui -extldflags=-static -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
echo "Built dicomqr.exe"
