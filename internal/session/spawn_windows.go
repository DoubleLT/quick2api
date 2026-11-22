//go:build windows

package session

import (
	"os/exec"
	"syscall"
)

// setSpawnAttrs hides the agent's console window and detaches it from our
// process group so closing quick2api's console can't cascade-kill it, while
// Close() can still terminate it explicitly.
func setSpawnAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
