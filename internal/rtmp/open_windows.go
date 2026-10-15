//go:build windows

package rtmp

import (
	"os"
	"syscall"
)

// openShared opens path for reading with FILE_SHARE_DELETE as well as the read/write sharing os.Open
// gives: hlsenc's delete of an old segment succeeds while the segment is being read (the file goes when
// the read ends) instead of leaving it behind. A rename over the open file still fails (see readFile).
func openShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
