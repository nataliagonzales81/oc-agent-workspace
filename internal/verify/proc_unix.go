//go:build unix

package verify

import (
	"os/exec"
	"syscall"
)

// isolate puts the gate in its own process group.
//
// Without this, killing the gate kills only the gate. A command that runs a
// child — and a test runner runs children — leaves that child holding the
// output pipe open, so cmd.Wait blocks on the copy until the child exits on its
// own. `sleep 30` under a shell would then sit there for 30 seconds after a
// 10-second deadline, and the timeout would be a suggestion.
func isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup kills the gate and everything it started, and waits briefly for
// them to go.
//
// SIGKILL rather than SIGTERM: a gate that ignored the deadline will probably
// ignore a polite request too, and the whole point of the deadline is that it
// is not negotiable.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// The negative pid is the process group. If the group is already gone this
	// is ESRCH, which is the outcome we wanted anyway.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
