# Run from Windows with Go 1.20.14 and an MSVCRT MinGW C compiler on PATH.
param(
    [ValidateSet("amd64", "386")]
    [string]$Arch = "amd64",
    [string]$Compiler = "gcc"
)
$ErrorActionPreference = "Stop"
if ((& go version) -notmatch '^go version go1\.20\.14 ') {
    throw "Windows 7 builds require Go 1.20.14."
}
if ($LASTEXITCODE -ne 0) { throw "Cannot run go." }
& $Compiler --version
if ($LASTEXITCODE -ne 0) { throw "Cannot run the MSVCRT MinGW C compiler." }
$crtMacros = & $Compiler -dM -E -include _mingw.h -x c NUL
if ($LASTEXITCODE -ne 0) { throw "Cannot inspect the MinGW C runtime headers." }
if ($crtMacros -match '^#define _UCRT(\s|$)') {
    throw "Use an MSVCRT toolchain for Windows 7 builds."
}
$env:GOOS = "windows"
$env:GOARCH = $Arch
$env:CGO_ENABLED = "1"
$env:CC = $Compiler
$env:CGO_CFLAGS = "$env:CGO_CFLAGS -D_WIN32_WINNT=0x0601 -DWINVER=0x0601"
$env:CGO_LDFLAGS = "$env:CGO_LDFLAGS -static"
$repoDir = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Push-Location $repoDir
try {
    New-Item -ItemType Directory -Force dist | Out-Null
    & go build -trimpath '-ldflags=-s -w' -o "dist/tiler_windows_$Arch.exe" .
    if ($LASTEXITCODE -ne 0) { throw "Windows build failed." }
} finally {
    Pop-Location
}
