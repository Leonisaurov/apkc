package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// appRun is a launched app: the stream apkc is following, plus what is needed to
// ask the device for the app's own log after that stream ended. An app that dies
// takes its stdout with it, so without the second question the last thing the
// user sees of a crash is nothing.
type appRun struct {
	cmd    *exec.Cmd
	answer deviceAnswer
	pkg    string
	marker string
}

// reportAfterEnd prints what the device still holds when the app is not running.
func (r *appRun) reportAfterEnd() {
	if pid, _ := r.answer("pidof -s " + r.pkg); isDigits(pid) {
		return
	}
	reportDeath(r.answer, r.pkg, r.marker, "the stream ended and the process is gone")
}

// runAdb (re)installs the apk over adb and runs the main activity
func runAdb() *appRun {
	// get list of all devices
	cmd := exec.Command(adbPath, "devices")
	out, err := cmd.Output()
	if err != nil {
		LogF("run", err, string(out))
	}

	devices := strings.Split(strings.TrimSpace(string(out)), "\n")[1:]
	if len(devices) == 0 {
		LogF("run", "no devices found")
	}

	// if there are more than one device prompt user to pick one
	var deviceNum int64 = 1
	if len(devices) > 1 {
		for i, d := range devices {
			fmt.Printf("[%d] %s\n", i+1, d)
		}
		deviceNum, err = strconv.ParseInt(prompt("Choose device to run app (1):", "1"), 10, 64)
		if err != nil {
			LogF("run", "invalid number")
		}
	}

	device := strings.Split(devices[deviceNum-1], "\t")
	serial := device[0]
	answer := adbAnswer(serial)

	// install apk in the device
	LogI("run", "installing in", device[1]+"("+device[0]+")")

	cmd = exec.Command(adbPath, "-s", serial, "install", "-r", filepath.Join("build", "app.apk"))
	out, err = cmd.CombinedOutput()
	if err != nil {
		LogF("run", err, string(out))
	}

	// collect package name and activity name from manifest
	pkg, activityName := mainActivityFromManifest()

	// the log window opens before the launch: an app that dies in onCreate leaves
	// no pid behind, and then the only way to its stack is the window itself
	marker := deviceTimestamp(answer)

	LogI("run", "launching main activity")
	out, err = exec.Command(adbPath, "-s", serial, "shell", fmt.Sprintf("am start -W -S -n %s/%s", pkg, activityName)).CombinedOutput()
	if err != nil {
		LogF("run", err, string(out))
	}
	// adb reports the outcome of the start in its answer, and its exit code says
	// nothing about the app
	if reason, failed := launchFailed(string(out)); failed {
		reportDeath(answer, pkg, marker, reason)
		LogF("run", "launch failed")
	}

	pid, _ := answer("pidof -s " + pkg)
	if !isDigits(pid) {
		reportDeath(answer, pkg, marker, "pidof found no process for "+pkg)
		LogF("run", "the app did not survive its start")
	}

	// stream logcat for the pid, on the device that was picked: -s belongs on
	// every call, or a second device makes adb refuse the launch and the log
	cmd = exec.Command(adbPath, "-s", serial, "logcat", "-v", "color", "--pid", pid)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		LogF("run", err)
	}
	return &appRun{cmd: cmd, answer: answer, pkg: pkg, marker: marker}
}

// deviceAnswer runs one command inside the device shell and returns its whole
// answer. Two implementations: adb against a serial, and the Shizuku shell. The
// error is only the transport: rish exits 0 whatever the device command did, so
// a caller has to judge the text.
type deviceAnswer func(script string) (string, error)

func adbAnswer(serial string) deviceAnswer {
	return func(script string) (string, error) {
		out, err := exec.Command(adbPath, "-s", serial, "shell", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
}

// deviceClock is the only shape logcat -T accepts for a starting point.
var deviceClock = regexp.MustCompile(`[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}`)

// deviceTimestamp is the device clock in the only format logcat -T reads. It is
// taken before the launch so the window can be asked for after the app died,
// without clearing anything the user still needs from before this run.
func deviceTimestamp(answer deviceAnswer) string {
	out, err := answer("date '+%m-%d %H:%M:%S.000'")
	if err != nil {
		LogD("run", "the device clock is not readable, the log window will be the whole buffer")
		return ""
	}

	// only the clock shape travels on: the marker is quoted inside a command run
	// on the device, so nothing that is not MM-DD HH:MM:SS.mmm may reach it
	return deviceClock.FindString(out)
}

// logcatWindow is the dump of what the device still holds since the marker: the
// crash buffer first, because it is small and holds exactly the stack of a start
// that never finished, then the lines of the main buffer that belong to the app.
func logcatWindow(answer deviceAnswer, pkg, marker string) []string {
	from := ""
	if marker != "" {
		from = " -T '" + marker + "'"
	}

	crash, _ := answer("logcat -d -b crash -v threadtime" + from)
	window, _ := answer("logcat -d -v threadtime" + from)
	live, _ := answer("pidof -s " + pkg)

	// the crash block is only suppressed down below when it is this app's: another
	// process dying in the same window is no reason to lose our own stack
	text := strings.TrimSpace(crash)
	report := appWindowLines(window, pkg, live, text != "" && strings.Contains(text, pkg))
	lines := []string{}
	if text != "" {
		lines = append(lines, text)
	}
	return append(lines, report...)
}

var (
	// procStart and procDeath name the pid the app had, from the ActivityManager
	// lines around it: pidof cannot answer once the process is gone, and those
	// lines are the only record of what it was while it lived.
	procStart  = regexp.MustCompile(`Start proc ([0-9]+)`)
	procDeath  = regexp.MustCompile(`\(pid ([0-9]+)\)`)
	threadtime = regexp.MustCompile(`^[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}`)
)

// appWindowLines keeps the lines of a logcat threadtime dump that belong to the
// app: its live pid if it has one, any pid the ActivityManager names next to our
// package, and the continuation lines (stack traces) that follow such a record.
// A line without the threadtime prefix is not a new record, so it stays with the
// record before it, which is what keeps a FATAL EXCEPTION readable.
//
// Naming the package is not enough on its own. Measured on this phone, the report of
// a start that died in onCreate ran to 163 lines, and the reason was in ten of them:
// the power hal, the window manager, the surface flinger and the vendor's game
// detector all have something to say about a package name, and none of it is what
// happened to the app. So a record from another process is kept only when it is the
// ActivityManager describing our lifecycle, or an error about us.
//
// Two records are left out on purpose. The shell that answers us writes its own log
// of the calls this run made, and a report holding them is describing itself. And the
// crash records are the main buffer's copy of what the crash buffer just printed,
// which the caller says with crashShown.
func appWindowLines(dump, pkg, livePid string, crashShown bool) []string {
	pids := map[string]bool{}
	if pid := strings.TrimSpace(livePid); isDigits(pid) {
		pids[pid] = true
	}

	lines := []string{}
	ours := false
	for _, line := range strings.Split(dump, "\n") {
		if !threadtime.MatchString(line) {
			if ours && strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 6 {
			ours = false
			continue
		}
		pid, level, tag := fields[2], fields[4], threadTag(fields)

		mine := pids[pid]
		if strings.Contains(line, pkg) && (lifecycle[tag] || level == "E" || level == "F") {
			mine = true
		}
		if tag == shellTag || (crashShown && tag == crashTag) {
			mine = false
		}
		if mine {
			for _, re := range []*regexp.Regexp{procStart, procDeath} {
				if found := re.FindStringSubmatch(line); found != nil {
					pids[found[1]] = true
				}
			}
		}
		ours = mine
		if mine {
			lines = append(lines, line)
		}
	}
	return lines
}

// lifecycle names the processes whose word about a package is about that package:
// the ones that start it, stop it, and finish its activities.
var lifecycle = map[string]bool{
	"ActivityManager":     true,
	"ActivityTaskManager": true,
}

// logcat pads a short tag to eight columns, which puts the colon in a field of its
// own, and leaves a long one glued to it. The tag is what says whose record this is.
func threadTag(fields []string) string {
	if len(fields) < 6 {
		return ""
	}
	return strings.TrimRight(fields[5], ":")
}

const (
	// shellTag is the Shizuku shell logging the very commands of this run.
	shellTag = "RISH"
	// crashTag is the record the runtime writes when the app dies.
	crashTag = "AndroidRuntime"
)

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// reportDeath says out loud that the app is not running, and prints what the
// device still holds about it. Without this the user of a start that crashed in
// onCreate gets "couldn't get the pid" and no line of the stack that caused it.
func reportDeath(answer deviceAnswer, pkg, marker string, reason string) {
	LogE("run", "the app is not running:", reason)
	lines := logcatWindow(answer, pkg, marker)
	if len(lines) == 0 {
		LogW("run", "the device kept nothing about", pkg, "in this window")
		return
	}

	LogI("run", "what the device holds about", pkg)
	for _, line := range lines {
		fmt.Println(line)
	}
}

// launchFailed reads the answer of `am start -W`, which reports its own outcome
// and does it in text: the exit code of the shell says nothing about the app.
func launchFailed(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "Status:") && !strings.Contains(text, "ok") {
			return text, true
		}
		// am writes "Error: type=..." and "Error type 3" and "Error: Activity
		// class ... does not exist.", so the prefix is the word, not the colon:
		// a start that never happened must not be streamed over
		if strings.HasPrefix(text, "Error") || strings.Contains(text, "Exception") {
			return text, true
		}
	}
	return "", false
}

// mainActivityFromManifest returns the package name and the launchable activity
// of the project, without touching the device.
func mainActivityFromManifest() (string, string) {
	mb, err := os.ReadFile("AndroidManifest.xml")
	if err != nil {
		LogF("run", err)
	}

	manifest := Manifest{}
	if err := xml.Unmarshal(mb, &manifest); err != nil {
		LogF("run", err)
	}
	activityName := manifest.Application.MainActivity()
	if activityName == "" {
		LogF("run", "couldn't find main activity")
	}

	return manifest.Package, activityName
}

// rishStage is the path the device shell writes and reads the apk from
const rishStage = "/data/local/tmp/.test-app.apk"

// rish runs a command inside the device shell and returns its output. The
// Shizuku shell exits 0 whatever the command did, so a caller has to judge the
// returned text; err only covers rish itself failing to run.
func rish(script string) (string, error) {
	cmd := exec.Command("rish", "-c", script)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// stageApk pipes the built apk into the device shell. Termux can neither write
// /data/local/tmp (owned by the shell uid) nor reach /sdcard (needs the storage
// permission), so the file travels through rish's stdin instead of a path.
func stageApk() {
	path := filepath.Join("build", "app.apk")
	info, err := os.Stat(path)
	if err != nil {
		LogF("run", err)
	}

	apk, err := os.Open(path)
	if err != nil {
		LogF("run", err)
	}
	defer apk.Close()

	cmd := exec.Command("rish", "-c", "cat > "+rishStage+" && chmod 644 "+rishStage)
	cmd.Stdin = apk
	if out, err := cmd.CombinedOutput(); err != nil {
		LogF("run", err, string(out))
	}

	// a broken pipe would leave a truncated apk that pm still tries to install
	size, err := rish("wc -c < " + rishStage)
	if err != nil {
		LogF("run", err, size)
	}
	if size != strconv.FormatInt(info.Size(), 10) {
		LogF("run", "the device shell received", size, "of", info.Size(), "bytes")
	}
}

// runRish (re)installs apk through the Shizuku shell and streams its logcat
func runRish() *appRun {
	if _, err := exec.LookPath("rish"); err != nil {
		LogF("run", "rish not found. enable the Shizuku shell first")
	}
	if !exists(filepath.Join("build", "app.apk")) {
		LogF("run", "build/app.apk is missing. build the apk before running with rish")
	}

	stageApk()

	// -r keeps the app data and allows reinstalling over the running copy
	out, err := rish("pm install -r " + rishStage)
	if err != nil || strings.Contains(out, "Failure") || strings.Contains(out, "Error") {
		rish("rm -f " + rishStage)
		LogF("run", "install failed:", out)
	}
	rish("rm -f " + rishStage)

	pkg, activityName := mainActivityFromManifest()

	// the window opens before the launch, so a start that died in onCreate still
	// has its stack in the buffer to be asked for
	marker := deviceTimestamp(rish)

	LogI("run", "launching main activity")
	out, err = rish(fmt.Sprintf("am start -W -S -n %s/%s", pkg, activityName))
	if err != nil {
		LogF("run", "the Shizuku shell did not answer:", out)
	}
	if reason, failed := launchFailed(out); failed {
		reportDeath(rish, pkg, marker, reason)
		LogF("run", "launch failed")
	}

	// the process may still be starting when am answers, so the pid is waited
	// for; if it never shows up the app is not running, and the window says why
	pid := ""
	for i := 0; i < 5; i++ {
		pid, _ = rish("pidof -s " + pkg)
		if isDigits(pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !isDigits(pid) {
		reportDeath(rish, pkg, marker, "no process for "+pkg+" after a second of trying")
		LogF("run", "the app did not survive its start")
	}

	// stream logcat for the pid, and stop with the process: see rishStream
	cmd := exec.Command("rish", "-c", rishStream(pid, pkg))
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		LogF("run", err)
	}
	return &appRun{cmd: cmd, answer: rish, pkg: pkg, marker: marker}
}

// rishStream is the log stream with a supervisor written into the device shell.
//
// adb's logcat quits when the process it follows dies and the run ends there; on
// this device, through the Shizuku shell, logcat --pid keeps the session open after
// the app is gone. The stream then produces nothing, the run waits on it forever,
// and the report of why the app stopped is never reached unless the user interrupts
// it. Asking the phone to end the stream when the pid is no longer the one we
// followed costs one pidof per second on the device and no extra session on Termux,
// which is the other way round from watching the pid from here.
func rishStream(pid, pkg string) string {
	return "logcat -v color --pid " + pid + " & stream=$!; " +
		"while [ \"$(pidof -s " + pkg + ")\" = \"" + pid + "\" ]; do sleep 1; done; " +
		"kill $stream 2>/dev/null"
}

// run builds the apk, installs it and follows the app output until it ends
func run() {
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	flags := registerBuildFlags(runCmd, false)
	useRish := runCmd.Bool("rish", false, "install and follow the app through the Shizuku shell instead of adb")

	runCmd.Parse(os.Args[2:])

	_build(flags.opts())

	var started *appRun
	if *useRish {
		started = runRish()
	} else {
		started = runAdb()
	}
	started.cmd.Wait()
	// the stream ends when the app dies, and the app is what the user was
	// waiting to hear from: ask the device for its log before saying nothing
	started.reportAfterEnd()
}

// isGenerated reports whether a path is apkc output or tool state. Watching
// those would make every build trigger the next one forever.
func isGenerated(path string) bool {
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == "build" || strings.HasPrefix(part, ".") {
			return true
		}
	}
	// bundleRes writes R.java and Manifest.java inside src/, both are products
	// of the resource link, not sources of the project
	return genJavaFile(filepath.Base(path))
}

// addRecursive watches every directory under root. A directory that cannot be
// read or added is reported and left alone: one dangling symlink, or one tree
// owned by another uid, must not take the whole watcher down.
func addRecursive(w *fsnotify.Watcher, root string) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			LogW("watch", "not watching", path, err.Error())
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && isGenerated(d.Name()) {
			return filepath.SkipDir
		}
		if err := w.Add(path); err != nil {
			LogW("watch", "not watching", path, err.Error())
		}
		return nil
	})
}

func watch() {
	watchCmd := flag.NewFlagSet("watch", flag.ExitOnError)
	flags := registerBuildFlags(watchCmd, true)
	useRish := watchCmd.Bool("rish", false, "install and follow the app through the Shizuku shell instead of adb")
	runEach := watchCmd.Bool("run", false, "install and follow the app after every rebuild")
	watchCmd.Parse(os.Args[2:])

	o := flags.opts()

	if watchUnavailable != "" {
		LogF("watch", watchUnavailable)
	}

	// every round is a separate apkc process: a failed build aborts its own
	// process with LogF, and the watcher survives to take the next change
	round := []string{"build"}
	args := []string{}
	if *runEach {
		round = []string{"run"}
	}
	if o.incremental {
		args = append(args, "--inc")
	}
	if o.coreLibrary {
		args = append(args, "--core-library")
	}
	// only what the user asked for that is not what the child would do anyway: the
	// value here is still the raw flag, because auto is resolved inside the build
	if o.signer != signerDefault {
		args = append(args, "--signer", o.signer)
	}
	if *useRish {
		args = append(args, "--rish")
	}
	if o.useAAB && !*runEach {
		args = append(args, "--aab")
	}
	if o.useAAB && *runEach {
		// run installs an apk, a bundle is not installable as is
		LogW("watch", "--aab is ignored with --run, building apk instead")
	}
	args = append(args, "--keystore", o.keyStore, "--storepass", o.storePass,
		"--keyalias", o.keyAlias, "--sigalg", o.sigAlg)

	self, err := os.Executable()
	if err != nil {
		LogF("watch", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		LogF("watch", "NewWatcher", err.Error())
	}
	defer w.Close()

	addRecursive(w, ".")

	debounce := 500 * time.Millisecond
	var timer *time.Timer
	var timeChan <-chan time.Time

	// One round at a time. With --run the round is the app's own log stream and a
	// new change replaces it at once; a plain build is left to finish, because
	// killing it halfway throws away the work it already paid for. The events that
	// arrive while a round runs are not replayed one by one from the queue: they
	// set pending, and the next round covers all of them together.
	var cur *exec.Cmd
	var finished <-chan error
	var pending bool
	var roundNumber int

	startRound := func() {
		if cur != nil {
			if !*runEach {
				pending = true
				return
			}
			stopRound(cur)
			cur, finished = nil, nil
		}

		roundNumber++
		// one line per round: without it the log says that a file changed and
		// then goes quiet, which looks exactly like a watcher that lost the event
		LogI("watch", "round", roundNumber, "started")

		roundCmd := exec.Command(self, append(round, args...)...)
		roundCmd.SysProcAttr = roundProcAttr
		roundCmd.Stdin = os.Stdin
		roundCmd.Stdout = os.Stdout
		roundCmd.Stderr = os.Stderr
		if err := roundCmd.Start(); err != nil {
			LogE("watch", err.Error())
			return
		}

		// buffered by one: a round replaced before this reads its exit still has
		// somewhere to put it, so the waiter never blocks
		done := make(chan error, 1)
		go func() { done <- roundCmd.Wait() }()
		cur, finished = roundCmd, done
	}

	// the closure matters: defer would otherwise capture cur while it is nil
	defer func() { stopRound(cur) }()

	startRound()

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}

			// a new subfolder has to be watched too
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					addRecursive(w, ev.Name)
					continue
				}
			}

			if isGenerated(ev.Name) {
				continue
			}

			// touch and editors both count: Chmod is what a plain touch emits,
			// Remove has to purge the artifacts of a deleted source
			if ev.Op&(fsnotify.Write|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod|fsnotify.Create) != 0 {
				fmt.Print("\n\r")
				LogI("Watcher", "Detected a file changed: ", ev.Name)
				if timer == nil {
					timer = time.NewTimer(debounce)
					timeChan = timer.C
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(debounce)
				}
			}

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			LogE("watch", err.Error())

		case err := <-finished:
			// finished is nil while no round runs, and a nil channel simply never
			// fires, so this case is the only place a round is retired
			cur, finished = nil, nil
			if err != nil && !*runEach {
				LogE("watch", "build failed, waiting for another change")
			}
			if pending {
				pending = false
				startRound()
			}

		case <-timeChan:
			timer, timeChan = nil, nil
			startRound()
		}
	}
}
