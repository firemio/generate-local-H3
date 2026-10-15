//go:build windows

// Package winproc holds the per-OS bits of launching child processes.
package winproc

import (
	"os/exec"
	"syscall"
)

// NoWindow keeps a child console program from opening its own window when the station itself runs hidden
// (scripts/run_station.ps1 starts it with -WindowStyle Hidden). The child still inherits our stdio handles.
func NoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW (not in package syscall)
}
