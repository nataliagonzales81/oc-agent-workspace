//go:build !unix

package verify

import "os/exec"

// isolate is a no-op where process groups are not the mechanism. The deadline
// still holds — see the WaitDelay in run.go — so a gate cannot hang ocaw; it
// can only leave a grandchild behind.
func isolate(cmd *exec.Cmd) {}

// killGroup falls back to killing the gate alone.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
