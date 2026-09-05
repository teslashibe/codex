//go:build !darwin && !linux

package codex

import "os/exec"

// WaitDelay bounds pipe cleanup on other platforms, but descendant processes
// are not killed as a group.
func configureProcessCleanup(cmd *exec.Cmd) func() {
	return func() {}
}
