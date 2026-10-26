package main

import (
	"errors"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/hashicorp/go-version"
)

var (
	// java sdk paths
	javaPath    string
	javaBinPath string
	javacPath   string
	javaCmdPath string
	// jarsigner is what signs a bundle: the same bin directory as javac, and
	// nothing else on this device has it under that name
	jarsignerPath string

	// kotlin sdk paths
	kotlincPath string

	// bundletool is not part of build-tools: it is its own install, either an
	// executable on PATH or a jar apkc has to start with java
	bundletoolPath string
	// bundletoolIsJar records which of the two the path above is
	bundletoolIsJar bool

	// android sdk paths
	sdkPath       string
	toolsPath     string
	zipAlignPath  string
	aapt2Path     string
	d8Path        string
	apksignerPath string
	adbPath       string

	// android java apis
	androidJar string

	// debug key store file for signing
	keyStorePath string
)

// doctor command checks all the sdk and binary paths are valid
// formatBuildStamp is the answer to "which apkc produced this number", said in the words
// a log can be searched for. It exists because two rounds of verification in this
// project were spent on output from an old release that was still in PATH, and nothing
// the tool printed could tell them apart: a build made from a checkout names its commit
// and says whether the tree was dirty when it was compiled, and a build made any other
// way says that instead of pretending.
func formatBuildStamp(settings []debug.BuildSetting, modulePath string) string {
	revision, modified, built := "", "", ""
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		case "vcs.time":
			built = s.Value
		}
	}
	if revision == "" {
		if modulePath == "" {
			return "no commit information in this build"
		}
		return "no commit information in this build (" + modulePath + ")"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	stamp := revision
	if built != "" {
		stamp += ", built " + built
	}
	if modified == "true" {
		stamp += ", with files not committed"
	}
	return stamp
}

// buildStamp reads what go embedded in this binary at compile time. A build that did not
// come from a module at all answers with nothing to show, which is the same answer as a
// build with no version control: the point is to never print a commit that is not this
// binary's.
func buildStamp() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return formatBuildStamp(nil, "")
	}
	return formatBuildStamp(info.Settings, info.Main.Path)
}

func doctor() {
	findSDKs()

	LogI("doctor", "this build:", buildStamp())
	LogI("doctor", "java", javaPath)
	LogI("doctor", "javac", javacPath)

	LogI("doctor", "kotlinc", kotlincPath)

	LogI("doctor", "sdk", sdkPath)
	LogI("doctor", "aapt2", aapt2Path)
	LogI("doctor", "d8", d8Path)
	LogI("doctor", "zipalign", zipAlignPath)
	LogI("doctor", "apksigner", apksignerPath)

	LogI("doctor", "android jar", androidJar)

	// what a build and a run need that no SDK search can answer: the key the
	// packages are signed with, and which of the two device backends is reachable
	if exists(keyStorePath) {
		LogI("doctor", "keystore", keyStorePath)
	} else {
		LogW("doctor", "no debug keystore at", keyStorePath, "- nothing can be signed until it exists")
	}

	if _, err := exec.LookPath(adbPath); err == nil {
		LogI("doctor", "adb", adbPath)
	} else {
		LogD("doctor", "no adb: apkc run without --rish cannot reach a device")
	}

	// the .aab path needs two things no SDK search answers by itself
	if bundletoolPath == "" {
		LogD("doctor", "no bundletool: apkc build --aab cannot assemble a bundle")
	} else if bundletoolIsJar {
		LogI("doctor", "bundletool", bundletoolPath, "started with", javaCmdPath)
	} else {
		LogI("doctor", "bundletool", bundletoolPath)
	}
	if exists(jarsignerPath) || jarsignerPath == "jarsigner" {
		LogI("doctor", "jarsigner", jarsignerPath)
	} else {
		LogW("doctor", "no jarsigner at", jarsignerPath, "- a bundle cannot be signed without it")
	}
	if _, err := exec.LookPath("rish"); err == nil {
		LogI("doctor", "rish", "on PATH (Shizuku shell: apkc run --rish)")
	} else {
		LogD("doctor", "no rish: start the Shizuku shell to run and watch without adb")
	}
}

// findSDKs finds Android SDK, Java SDK and paths of other required binaries
func findSDKs() {
	home, err := os.UserHomeDir()
	if err != nil {
		LogF("doctor", err)
	}
	keyStorePath = filepath.Join(home, ".android", "debug.keystore")

	sdkPath = os.Getenv("ANDROID_HOME")

	if sdkPath == "" {
		LogW("doctor", "ANDROID_HOME was not found in environment")
		// try default paths
		switch runtime.GOOS {
		case "darwin":
			sdkPath = filepath.Join(home, "Android", "Sdk")
		case "linux":
			sdkPath = filepath.Join(home, "Library", "Android", "sdk")
		case "windows":
			sdkPath = filepath.Join(home, "AppData", "Local", "Android", "sdk")
		default:
			LogW("doctor", "SDK path unknown")
		}
	}

	if sdkPath != "" {
		btVersion, err := latestBuildTools(filepath.Join(sdkPath, "build-tools"))
		if err != nil {
			LogE("doctor", err)
		}
		toolsPath = filepath.Join(sdkPath, "build-tools", btVersion)
		aapt2Path = filepath.Join(toolsPath, "aapt2")
		d8Path = filepath.Join(toolsPath, "d8")
		zipAlignPath = filepath.Join(toolsPath, "zipalign")
		apksignerPath = filepath.Join(toolsPath, "apksigner")
		adbPath = filepath.Join(sdkPath, "platform-tools", "adb")
		api := strings.Split(btVersion, ".")[0]
		androidJar = filepath.Join(sdkPath, "platforms", "android-"+api, "android.jar")
	}

	javaPath = os.Getenv("JAVA_HOME")
	if javaPath != "" {
		javaBinPath = filepath.Join(javaPath, "bin")
		javacPath = filepath.Join(javaBinPath, "javac")
		javaCmdPath = filepath.Join(javaBinPath, "java")
		jarsignerPath = filepath.Join(javaBinPath, "jarsigner")
	} else {
		LogW("doctor", "JAVA_HOME was not found in environment")
		// hope the bin is in path
		javacPath = "javac"
		javaCmdPath = "java"
		jarsignerPath = "jarsigner"
	}

	kotlincPath = "kotlinc"

	findBundletool()
}

// findBundletool answers how the bundle is assembled on this machine. bundletool
// ships as a jar and distributions install it in different shapes, so both are
// accepted: an executable called bundletool, or a jar somewhere under the SDK or the
// home directory, which apkc then starts with java. The search is bounded on purpose
// - walking a home tree on this device costs more than the one round that needs it.
func findBundletool() {
	if p, err := exec.LookPath("bundletool"); err == nil {
		bundletoolPath, bundletoolIsJar = p, false
		return
	}

	bases := []string{}
	if sdkPath != "" {
		bases = append(bases, filepath.Join(sdkPath, "bundletool"), sdkPath)
	}
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases, filepath.Join(home, "Android", "bundletool"))
	}

	for _, base := range bases {
		matches, err := filepath.Glob(filepath.Join(base, "bundletool*.jar"))
		if err != nil {
			continue
		}
		if len(matches) == 0 && base != "" {
			// one level down, which is where a versioned directory puts it
			matches, _ = filepath.Glob(filepath.Join(base, "*", "bundletool*.jar"))
		}
		sort.Strings(matches)
		if len(matches) > 0 {
			bundletoolPath, bundletoolIsJar = matches[len(matches)-1], true
			return
		}
	}
}

// latestBuildTools checks the available build tools and picks the most recent one
func latestBuildTools(path string) (string, error) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return "", err
	}

	versions := []*version.Version{}
	for _, f := range files {
		n := f.Name()
		// exclude release candidates
		if f.IsDir() && !strings.Contains(n, "rc") {
			v, err := version.NewVersion(n)
			if err != nil {
				LogW("doctor", "error parsing build tools version '"+n+"'")
				continue
			}
			versions = append(versions, v)
		}
	}

	if len(versions) == 0 {
		return "", errors.New("no usable build tools versions")
	}

	sort.Sort(version.Collection(versions))

	return versions[len(versions)-1].String(), nil
}
