//go:build unix

package youtube

import (
	"os/exec"
	"syscall"
)

// stopWithChildren makes command run in a process group of its own and kill the whole group when its context ends.
// yt-dlp starts helpers (ffmpeg, a JavaScript runtime), which would otherwise live on after a cancellation and keep
// its output open.
func stopWithChildren(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = killWaitDelay
}
