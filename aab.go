package main

import (
	"os/exec"
	"path/filepath"
)

// buildAAB turns build/bundle.zip into build/app.aab with bundletool. bundletool
// is not part of the build-tools directory the rest of apkc resolves: it is its
// own install, and the check that was here looked at cmd.Err before the command
// had started, which is a field nothing sets until Start runs.
func buildAAB() {
	bundletool, err := exec.LookPath("bundletool")
	if err != nil {
		LogF("build", "bundletool is not on PATH, and an .aab cannot be built without it",
			"install the Android bundletool")
	}

	cmd := exec.Command(bundletool, "build-bundle", "--overwrite", "--modules=bundle.zip", "--output=app.aab")
	cmd.Dir = "build"
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}
}

// signAAB signs aab with jarsigner and provided debug keys
func signAAB(keyStore, storePass, keyAlias, sigAlg string) {
	LogI("build", "signing aab")

	// cmd := exec.Command("jarsigner", "-verbose", "-sigalg", "SHA256withRSA", "pass:"+storePass, "--ks", keyStore, "--ks-key-alias", keyAlias, filepath.Join("build", "app.apk"))
	cmd := exec.Command("jarsigner", "-verbose", "-sigalg", sigAlg, "-digestalg", "SHA1", "-storepass", storePass, "-keystore", keyStore, filepath.Join("build", "app.aab"), keyAlias)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}
}
