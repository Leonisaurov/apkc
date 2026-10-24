//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// roundProcAttr has nothing to set here: process groups are a Unix idea, and on
// Windows the equivalent (job objects) is not worth a dependency for a tool that
// runs adb, which has no logcat to leak.
var roundProcAttr = &syscall.SysProcAttr{}

// stopRound kills a round. Grandchildren of the round are not reached, the same
// way Process.Kill does not reach them in any other Windows program.
func stopRound(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
