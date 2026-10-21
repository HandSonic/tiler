#!/usr/bin/env bash
# Cross-build full Windows 7 binaries, including SQLite (CGO).
# Use llvm-mingw's MSVCRT distribution, not its UCRT distribution.
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go_cmd="${GO_CMD:-go}"
if [[ "$("$go_cmd" version)" != "go version go1.20.14 "* ]]; then
  echo 'Windows 7 builds require Go 1.20.14. Set GO_CMD to that go executable.' >&2
  exit 1
fi
if [[ -z "${MINGW_ROOT:-}" ]]; then
  echo 'Set MINGW_ROOT to an extracted llvm-mingw MSVCRT toolchain.' >&2
  exit 1
fi
export PATH="$MINGW_ROOT/bin:$PATH"
# Also supports environments where the loader cannot resolve $ORIGIN.
export LD_LIBRARY_PATH="$MINGW_ROOT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export CGO_ENABLED=1 GOOS=windows
export CGO_CFLAGS="${CGO_CFLAGS:-} -D_WIN32_WINNT=0x0601 -DWINVER=0x0601"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -static"
cd "$repo_dir"
mkdir -p dist

for arch in amd64 386; do
  if [[ "$arch" == amd64 ]]; then
    export CC=x86_64-w64-mingw32-gcc
  else
    export CC=i686-w64-mingw32-gcc
  fi
  command -v "$CC" >/dev/null
  # UCRT adds a separate runtime requirement on Windows 7.
  if "$CC" -dM -E -include _mingw.h -x c /dev/null | grep -E '^#define _UCRT($|[[:space:]])' >/dev/null; then
    echo 'Use an MSVCRT toolchain for Windows 7 builds.' >&2
    exit 1
  fi
  GOARCH="$arch" "$go_cmd" build -trimpath -ldflags='-s -w' -o "dist/tiler_windows_${arch}.exe" .
done
