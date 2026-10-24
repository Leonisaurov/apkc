//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// roundProcAttr puts every round of watch in its own process group. With --run
// the round starts adb or rish logcat as a grandchild that inherits our stdout,
// and killing only the apkc child would leave that stream alive: after N rounds
// N logcats would be printing over each other.
var roundProcAttr = &syscall.SysProcAttr{Setpgid: true}

// stopRound kills a round and everything it started, then waits for it.
func stopRound(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// the negative pid addresses the group created by Setpgid
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
}
