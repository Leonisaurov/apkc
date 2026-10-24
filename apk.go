package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// alignAPK does zipalign
func alignAPK() {
	LogI("build", "running zipalign")

	cmd := exec.Command(zipAlignPath, "-f", "4", filepath.Join("build", "bundle.zip"), filepath.Join("build", "app.apk"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}

	// verify with: zipalign -c -v 4 build/app.apk
}

// signAPK signs apk with apksigner and provided debug keys
func signAPK(keyStore, storePass, keyAlias *string) {
	LogI("build", "signing app")

	args := []string{"sign", "--ks-pass", "pass:" + *storePass, "--ks", *keyStore, "--ks-key-alias", *keyAlias, filepath.Join("build", "app.apk")}
	// on Windows apksigner is a .bat wrapper that cannot be exec'd directly
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		javaArgs := append([]string{"-jar", filepath.Join(toolsPath, "lib", "apksigner.jar")}, args...)
		cmd = exec.Command(javaExe(), javaArgs...)
	} else {
		cmd = exec.Command(apksignerPath, args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}
}
