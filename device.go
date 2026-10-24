package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// run (re)installs apk and runs the main activity
func runAdb() *exec.Cmd {
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

	// install apk in the device
	LogI("run", "installing in", device[1]+"("+device[0]+")")

	cmd = exec.Command(adbPath, "-s", serial, "install", "-r", filepath.Join("build", "app.apk"))
	out, err = cmd.CombinedOutput()
	if err != nil {
		LogF("run", err, string(out))
	}

	// collect package name and activity name from manifest
	pkg, activityName := mainActivityFromManifest()

	// launch activity
	LogI("run", "launching main activity")
	cmd = exec.Command(adbPath, "shell", "am", "start", "-W", "-S", "-n", fmt.Sprintf("%s/%s", pkg, activityName))
	out, err = cmd.CombinedOutput()
	if err != nil {
		LogF("run", err, string(out))
	}

	// get pid of app
	cmd = exec.Command(adbPath, "shell", "pidof", "-s", pkg)
	out, err = cmd.Output()
	if err != nil {
		LogF("run", err, string(out))
	}
	pid := strings.TrimSpace(string(out))

	// stream logcat for the pid
	cmd = exec.Command(adbPath, "logcat", "-v", "color", "--pid", pid)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err = cmd.Start()
	if err != nil {
		LogF("run", err, string(out))
	}
	return cmd
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

// rishStage is the path the Shizuku shell can read the apk from
const rishStage = "/data/local/tmp/.test-app.apk"

// rish runs a command inside the device shell and returns its output
func rish(script string) (string, error) {
	cmd := exec.Command("rish", "-c", script)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// runRish (re)installs apk through the Shizuku shell and streams its logcat
func runRish() *exec.Cmd {
	if _, err := exec.LookPath("rish"); err != nil {
		LogF("run", "rish not found. enable the Shizuku shell first")
	}
	if !exists(filepath.Join("build", "app.apk")) {
		LogF("run", "build/app.apk is missing. build the apk before running with rish")
	}

	// /sdcard is the only location Termux and the device shell share
	cmd := exec.Command("cp", filepath.Join("build", "app.apk"), "/sdcard/.test-app.apk")
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("run", err, string(out))
	}

	if out, err := rish("mv /sdcard/.test-app.apk " + rishStage + " && chmod 644 " + rishStage); err != nil {
		LogF("run", err, out)
	}

	// -r keeps the app data and allows reinstalling over the running copy
	if out, err := rish("pm install -r " + rishStage); err != nil {
		rish("rm -f " + rishStage)
		LogF("run", err, out)
	}
	rish("rm -f " + rishStage)

	pkg, activityName := mainActivityFromManifest()

	// launch activity
	LogI("run", "launching main activity")
	if out, err := rish(fmt.Sprintf("am start -W -S -n %s/%s", pkg, activityName)); err != nil {
		LogF("run", err, out)
	}

	// get pid of app
	var pid string
	for i := 0; i < 5; i++ {
		cmd = exec.Command("rish", "-c", "pidof -s "+pkg)
		out, err = cmd.Output()
		pid = strings.TrimSpace(string(out))
		if pid != "" {
			break
		}
		LogE("run", "Can't get the app pid.")
		LogI("run", fmt.Sprintf("[%d] Retrying...", i+1))
	}
	if pid == "" {
		LogF("run", "couldn't get the pid of", pkg)
	}

	// stream logcat for the pid
	cmd = exec.Command("rish", "-c", "logcat -v color --pid "+pid)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err = cmd.Start()
	if err != nil {
		LogF("run", err, string(out))
	}
	return cmd
}

// run builds the apk, installs it and follows the app output until it ends
func run() {
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)

	useRish := runCmd.Bool("rish", false, "use rish to install the app instead of adb")
	incremental := runCmd.Bool("inc", false, "run an incremental build before installing")
	keyStore := runCmd.String("keystore", keyStorePath, "path to keystore")
	storePass := runCmd.String("storepass", "android", "keystore password")
	keyAlias := runCmd.String("keyalias", "androiddebugkey", "key alias to use")
	sigAlg := runCmd.String("sigalg", "SHA256withRSA", "signature to use")
	runCmd.Parse(os.Args[2:])

	_build(buildOpts{
		incremental: *incremental,
		keyStore:    *keyStore,
		storePass:   *storePass,
		keyAlias:    *keyAlias,
		sigAlg:      *sigAlg,
	})

	var cmd *exec.Cmd
	if *useRish {
		cmd = runRish()
	} else {
		cmd = runAdb()
	}
	cmd.Wait()
}

// stopCmd terminates a running device process, if there is one
func stopCmd(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// isGenerated reports whether a path is apkc output or tool state. Watching
// those would make every build trigger the next one forever.
func isGenerated(path string) bool {
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == "build" || strings.HasPrefix(part, ".") {
			return true
		}
	}
	// bundleRes writes R.java inside src/, it is a product of the resources
	return filepath.Base(path) == "R.java"
}

func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && isGenerated(d.Name()) {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

func watch() {
	watchCmd := flag.NewFlagSet("watch", flag.ExitOnError)

	useAAB := watchCmd.Bool("aab", false, "build aab instead of apk")
	incremental := watchCmd.Bool("inc", false, "use the incremental build system")
	useRish := watchCmd.Bool("rish", false, "use rish to install the app instead of adb")
	runEach := watchCmd.Bool("run", false, "install and follow the app after every rebuild")
	keyStore := watchCmd.String("keystore", keyStorePath, "path to keystore")
	storePass := watchCmd.String("storepass", "android", "keystore password")
	keyAlias := watchCmd.String("keyalias", "androiddebugkey", "key alias to use")
	sigAlg := watchCmd.String("sigalg", "SHA256withRSA", "signature to use")
	watchCmd.Parse(os.Args[2:])

	// every round is a separate apkc process: a failed build aborts its own
	// process with LogF, and the watcher survives to take the next change
	round := []string{"build"}
	args := []string{}
	if *runEach {
		round = []string{"run"}
	}
	if *incremental {
		args = append(args, "--inc")
	}
	if *useRish {
		args = append(args, "--rish")
	}
	if *useAAB && !*runEach {
		args = append(args, "--aab")
	}
	if *useAAB && *runEach {
		// run installs an apk, a bundle is not installable as is
		LogW("watch", "--aab is ignored with --run, building apk instead")
	}
	args = append(args, "--keystore", *keyStore, "--storepass", *storePass,
		"--keyalias", *keyAlias, "--sigalg", *sigAlg)

	self, err := os.Executable()
	if err != nil {
		LogF("watch", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		LogF("watch", "NewWatcher", err.Error())
	}
	defer w.Close()

	if err := addRecursive(w, "."); err != nil {
		LogF("watch", "Add", err.Error())
	}

	debounce := 500 * time.Millisecond
	var timer *time.Timer
	var timeChan <-chan time.Time

	var cmd *exec.Cmd
	defer stopCmd(cmd)

	rebuild := func() {
		stopCmd(cmd)
		cmd = nil

		roundCmd := exec.Command(self, append(round, args...)...)
		roundCmd.Stdin = os.Stdin
		roundCmd.Stdout = os.Stdout
		roundCmd.Stderr = os.Stderr
		if err := roundCmd.Start(); err != nil {
			LogE("watch", err.Error())
			return
		}
		if *runEach {
			// the app output keeps running until the next rebuild kills it
			cmd = roundCmd
			return
		}
		if err := roundCmd.Wait(); err != nil {
			LogE("watch", "build failed, waiting for another change")
		}
	}

	rebuild()

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}

			// a new subfolder has to be watched too
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					_ = addRecursive(w, ev.Name)
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

		case <-timeChan:
			timer, timeChan = nil, nil
			rebuild()
		}
	}
}
