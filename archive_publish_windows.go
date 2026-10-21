//go:build windows

package main

import "syscall"

// MoveFileW publishes within the destination volume without replacing any
// existing name. Unlike hard links, it also works on FAT/exFAT on Windows 7.
func archivePublishNoReplace(source, destination string) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return syscall.MoveFile(from, to)
}
