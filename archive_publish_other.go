//go:build !windows

package main

import "os"

// The staged file and output share a volume. Linking publishes the complete
// archive atomically and refuses a destination created during conversion.
func archivePublishNoReplace(source, destination string) error {
	return os.Link(source, destination)
}
