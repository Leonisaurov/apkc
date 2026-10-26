//go:build js || wasip1

// The file is not called proc_js.go on purpose: go reads a _js suffix as a constraint of
// its own, on top of the line above, and that combination would leave wasip1 with none of
// the three process files and the package without roundProcAttr.

package main

import (
	"os/exec"
	"syscall"
)

// There is no process to put in a group here: a build of apkc for the browser or for
// wasm starts no external program at all, which is the whole of what apkc does, so this
// file exists to make the architecture compile rather than to make it work. What it
// promises is that nothing leaks: watch kills its round with the same call that kills a
// process anywhere else, and there is no grandchild to reach because there is no child.
//
// The one thing this costs is watch: a watcher over a file system nobody writes, asked
// to rebuild by starting a program that cannot be started, has nothing left to report.
// The command says so instead of waiting for the first event to arrive and fail.
var watchUnavailable = "this build of apkc starts no external program, so there is nothing to rebuild on a change"

var roundProcAttr = &syscall.SysProcAttr{}

// stopRound kills a round and waits for it.
func stopRound(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
