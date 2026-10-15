//go:build !windows

package rtmp

import "os"

// openShared is os.Open where the sharing mode is not an issue (see open_windows.go).
func openShared(path string) (*os.File, error) { return os.Open(path) }
