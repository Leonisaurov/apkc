package main

import (
	"errors"
	"strings"
	"testing"
)

// TestRishStreamOutlivesTheAppUnlessSupervised pins what was measured on the phone:
// logcat --pid through the Shizuku shell keeps the session after the process dies,
// so a bare stream would leave the run waiting on a silent pipe and the death report
// unreachable. The script has to carry the pid it follows, the package to look for,
// and the kill that ends it.
func TestRishStreamOutlivesTheAppUnlessSupervised(t *testing.T) {
	script := rishStream("1234", "com.apkc.demo")

	for _, want := range []string{"logcat -v color --pid 1234", "pidof -s com.apkc.demo", `"1234"`, "kill $stream"} {
		if !strings.Contains(script, want) {
			t.Errorf("the stream script is missing %q: %s", want, script)
		}
	}
	// the loop runs on the device: watching the pid from here would open a rish
	// session per check, and one session costs more than the app's whole log
	if !strings.Contains(script, "sleep 1") {
		t.Errorf("the supervisor must wait on the device, got %s", script)
	}
}

func TestWatchIgnoresEveryFileAaptGenerates(t *testing.T) {
	for _, path := range []string{"src/com/myapp/R.java", "src/com/myapp/Manifest.java"} {
		if !isGenerated(path) {
			t.Errorf("%s is a product of aapt2 link: watching it makes the round that wrote it trigger the next", path)
		}
	}

	for _, path := range []string{"src/com/myapp/MainActivity.java", "src/com/myapp/App.kt", "src/com/myapp/Constants.java", "AndroidManifest.xml", "res/values/strings.xml"} {
		if isGenerated(path) {
			t.Errorf("%s is a source of the project and must trigger a rebuild", path)
		}
	}
}

func TestWatchIgnoresTheTreesApkcWrites(t *testing.T) {
	for _, path := range []string{"build/classes/com/myapp/App.class", "build/incremental.json", "src/com/myapp/gen/R.java", ".gradle/caches/x.jar"} {
		if !isGenerated(path) {
			t.Errorf("%s is tool state or output, it must not reach the watcher", path)
		}
	}
}

// startDump is a logcat threadtime window of an app that died in onCreate: by the
// time apkc asks, pidof has nothing to return, and the only trace of the crash is
// the pid the ActivityManager named next to the package.
const startDump = `10-24 10:00:00.100  1200  1200 I ActivityManager: Start proc 8341:com.myapp/u0a379 for top-activity {com.myapp/com.myapp.MainActivity}
10-24 10:00:00.200  8341  8341 I MyApp   : onCreate
10-24 10:00:00.250   900   900 W SystemServer: wifi scan started
10-24 10:00:00.300  8341  8341 E AndroidRuntime: FATAL EXCEPTION: main
10-24 10:00:00.300  8341  8341 E AndroidRuntime: Process: com.myapp, PID: 8341
10-24 10:00:00.300  8341  8341 E AndroidRuntime: java.lang.IllegalArgumentException: bad thing
	at com.myapp.MainActivity.onCreate(MainActivity.java:33)
10-24 10:00:00.400   900   900 I ActivityManager: Process com.myapp (pid 8341) has died
10-24 10:00:00.500   900   900 I ActivityManager: Killing 4321:other.app
	at other.app.Thing.go(Thing.java:9)
`

func TestAppWindowLinesFollowAnAppThatLeftNoPid(t *testing.T) {
	lines := appWindowLines(startDump, "com.myapp", "", false)

	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Start proc 8341", "onCreate", "FATAL EXCEPTION", "bad thing", "has died"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report lost %q, got:\n%s", want, joined)
		}
	}
	// the stack line printed without the threadtime prefix belongs to the record
	// before it, which was ours
	if !strings.Contains(joined, "at com.myapp.MainActivity.onCreate") {
		t.Errorf("the continuation of our own record must travel with it, got:\n%s", joined)
	}
	for _, unwanted := range []string{"wifi scan", "other.app", "at other.app.Thing.go"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("the report carries %q, which is another process's business", unwanted)
		}
	}
}

func TestAppWindowLinesTakeTheLivePidToo(t *testing.T) {
	lines := appWindowLines(startDump, "com.myapp", "8341", false)
	if len(lines) == 0 {
		t.Fatal("a running app has a window too")
	}
	for _, line := range lines {
		if strings.Contains(line, "wifi scan") {
			t.Error("the live pid does not make another process ours")
		}
	}
}

// TestAppWindowLinesLeaveTheShellEchoAndTheSecondStack pins two things measured on
// the phone. The Shizuku shell logs every command this run makes, so its records
// turn a report about the app into a narration of the tool. And once the crash buffer
// has been printed, the main buffer's copy of the same stack is a second stack.
func TestAppWindowLinesLeaveTheShellEchoAndTheSecondStack(t *testing.T) {
	dump := startDump + "10-24 10:00:00.600  4300  4300 D RISH    : arg2=pidof -s com.myapp\n" +
		"10-24 10:00:00.700   882   947 I libPowerHal: [perfNotifyAppState] pack:com.myapp, pid:8341, STATE_DEAD\n" +
		"10-24 10:00:00.800  1200  1200 W PackageManager: component com.myapp/.Old disabled\n" +
		"10-24 10:00:00.900   900   900 E SystemServer: crash reporting for com.myapp failed\n"

	joined := strings.Join(appWindowLines(dump, "com.myapp", "", true), "\n")
	for _, unwanted := range []string{"RISH", "FATAL EXCEPTION", "bad thing", "at com.myapp.MainActivity.onCreate", "perfNotifyAppState", "PackageManager"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("the report carries %q, which is the shell talking about us or a stack printed above:\n%s", unwanted, joined)
		}
	}
	for _, want := range []string{"Start proc 8341", "onCreate", "has died"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report lost %q, and that is not a duplicate", want)
		}
	}
	// an error from another process is about us even when that process is not ours
	if !strings.Contains(joined, "crash reporting for com.myapp failed") {
		t.Errorf("the report dropped another process's error about us:\n%s", joined)
	}

	// with nothing in the crash buffer the stack is the only thing the user gets, and
	// the shell echo is still not the app's log
	without := strings.Join(appWindowLines(dump, "com.myapp", "", false), "\n")
	for _, want := range []string{"FATAL EXCEPTION", "bad thing", "at com.myapp.MainActivity.onCreate"} {
		if !strings.Contains(without, want) {
			t.Errorf("with no crash buffer, the report lost %q", want)
		}
	}
	if strings.Contains(without, "RISH") {
		t.Error("the shell echo is never the app's log, crash buffer or not")
	}
}

func TestLaunchFailedReadsWhatAmReports(t *testing.T) {
	started := "Starting: Intent { cmp=com.myapp/.MainActivity }\nStatus: ok\nLaunchState: COLD\nTotalTime: 412\n"
	if _, failed := launchFailed(started); failed {
		t.Error("a successful start is not a failure")
	}

	for _, out := range []string{
		"Status: error\nError type: UNKNOWN",
		"Error type 3\n",
		"Error: type=3 msg=Activity class {com.myapp/com.missing.MainActivity} does not exist.",
		"Error: Activity class {com.myapp/com.myapp.Missing} does not exist.\n",
		"Exception occurred while launching: java.lang.SecurityException",
	} {
		if _, failed := launchFailed(out); !failed {
			t.Errorf("this start did not happen and it was read as fine:\n%s", out)
		}
	}

	// a warning is not a failure: the intent was delivered, the app is running
	for _, out := range []string{
		"Warning: Activity not started, intent has been delivered to currently running top-most instance.\nStatus: ok\n",
	} {
		if reason, failed := launchFailed(out); failed {
			t.Errorf("a delivered intent was read as a failure: %q", reason)
		}
	}
}

func TestDeviceTimestampCarriesOnlyWhatLogcatReads(t *testing.T) {
	// the marker goes inside single quotes in a device command, so nothing that
	// is not a date or clock character may survive the trip
	answer := func(string) (string, error) { return "10-24 10:03:07.400'; rm -rf /\n", nil }
	got := deviceTimestamp(answer)
	if got != "10-24 10:03:07.400" {
		t.Errorf("marker = %q, want the clock only", got)
	}

	empty := func(string) (string, error) { return "", errors.New("no answer") }
	if got := deviceTimestamp(empty); got != "" {
		t.Errorf("an unreadable clock must fall back to no window, got %q", got)
	}
}
