package main

import (
	"os/exec"
	"path/filepath"
)

// buildAAB turns build/bundle.zip into build/app.aab with bundletool. bundletool
// is not part of the build-tools directory the rest of apkc resolves: it is its
// own install, and the check that was here looked at cmd.Err before the command
// had started, which is a field nothing sets until Start runs.
//
// It arrives on a machine either as an executable or as a jar, so both shapes go
// through the same call: the jar one starts it with the java that javac came from,
// which is the only reason the JDK is needed for a bundle at all.
func buildAAB() {
	if bundletoolPath == "" {
		LogF("build", "no bundletool found, and an .aab cannot be assembled without it",
			"put bundletool on PATH or a bundletool-*.jar under the SDK or ~/Android/bundletool")
	}

	args := []string{"build-bundle", "--overwrite", "--modules=bundle.zip", "--output=app.aab"}
	if bundletoolIsJar {
		args = append([]string{"-jar", bundletoolPath}, args...)
		cmd := exec.Command(javaCmdPath, args...)
		cmd.Dir = "build"
		runOrAbort(cmd)
		return
	}

	cmd := exec.Command(bundletoolPath, args...)
	cmd.Dir = "build"
	runOrAbort(cmd)
}

// signAAB signs the bundle with jarsigner. A bundle is not an apk: it is never
// zipaligned and never carries an APK Signing Block, so the JAR signature is the
// only one it has, and the JVM tool is the one that writes it.
//
// The digest is SHA-256 and not the SHA1 this used to ask for: the JDK that ships
// today reports SHA-1 as a security risk and disables it, so a signature written
// with it is a signature the toolchain itself warns about.
func signAAB(keyStore, storePass, keyAlias, sigAlg string) {
	LogI("build", "signing aab")

	cmd := exec.Command(jarsignerPath, "-verbose", "-sigalg", sigAlg, "-digestalg", "SHA-256",
		"-storepass", storePass, "-keystore", keyStore, filepath.Join("build", "app.aab"), keyAlias)
	runOrAbort(cmd)
}

// runOrAbort runs a tool and shows its whole answer when it fails: a bundletool
// refusal is a wall of text about the module set, and the last three lines of it
// are not the reason.
func runOrAbort(cmd *exec.Cmd) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), cmd.Args[0])
	}
	if len(out) > 0 {
		LogD("build", string(out))
	}
}
