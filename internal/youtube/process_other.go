//go:build !unix

package youtube

import "os/exec"

// stopWithChildren only limits how long a canceled command is waited for; killing its helpers needs process groups.
func stopWithChildren(command *exec.Cmd) {
	command.WaitDelay = killWaitDelay
}
