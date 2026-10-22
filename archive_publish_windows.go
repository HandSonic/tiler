//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"syscall"
)

// MoveFileW publishes within the destination volume without replacing any
// existing name. Unlike hard links, it also works on FAT/exFAT on Windows 7.
func archivePublishNoReplace(source, destination string) error {
	from, err := archiveWindowsMovePath(source)
	if err != nil {
		return err
	}
	to, err := archiveWindowsMovePath(destination)
	if err != nil {
		return err
	}
	return syscall.MoveFile(from, to)
}

func archiveWindowsMovePath(path string) (*uint16, error) {
	// Explicit extended/device paths already specify their own interpretation.
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return syscall.UTF16PtrFromString(path)
	}
	// Extended paths do not resolve relative paths, slash separators or dot
	// components. Resolve those before adding the prefix.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	encoded, err := syscall.UTF16FromString(absolute)
	if err != nil {
		return nil, err
	}
	// MoveFileW's MAX_PATH includes the terminating NUL. Windows 7 requires
	// the extended prefix beyond this limit; keep short paths in Win32 form.
	// https://learn.microsoft.com/windows/win32/api/winbase/nf-winbase-movefilew
	const maxPathUTF16Units = 260
	if len(encoded) <= maxPathUTF16Units || strings.HasPrefix(absolute, `\\?\`) || strings.HasPrefix(absolute, `\\.\`) {
		return &encoded[0], nil
	}
	if strings.HasPrefix(absolute, `\\`) {
		absolute = `\\?\UNC\` + absolute[len(`\\`):]
	} else {
		absolute = `\\?\` + absolute
	}
	return syscall.UTF16PtrFromString(absolute)
}
