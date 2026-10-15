//go:build !windows

package winproc

import "os/exec"

// NoWindow is a no-op outside Windows.
func NoWindow(cmd *exec.Cmd) {}
