//go:build !windows

package session

import "os/exec"

// setSpawnAttrs is a no-op outside Windows.
func setSpawnAttrs(cmd *exec.Cmd) {}
