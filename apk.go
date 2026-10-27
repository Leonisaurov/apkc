package main

import (
	"os/exec"
	"path/filepath"
)

// alignAPK does zipalign. pageAlign is the other half of
// android:extractNativeLibs="false": -p puts every uncompressed .so on a page
// boundary, which is what lets the runtime map it out of the apk instead of
// unpacking it at install.
func alignAPK(pageAlign bool) {
	defer timeIt("zipalign")()
	LogI("build", "running zipalign")

	args := []string{"-f"}
	if pageAlign {
		args = append(args, "-p")
	}
	args = append(args, "4", filepath.Join("build", "bundle.zip"), filepath.Join("build", "app.apk"))

	cmd := exec.Command(zipAlignPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", toolFailure("zipalign", out, err), zipAlignPath)
	}

	// verify with: zipalign -c -v 4 build/app.apk
}

// signAPK signs apk with apksigner and provided debug keys.
//
// --align-file-size=false was tried and reverted. On the demo with the largest package
// here (MnnChat: 136 MiB of assets, a 142 MB apk, one source edited per cycle) signing
// cost 11.09s and 13.04s with the flag against 8.24s without it, and the signed package
// came out 3013 bytes *bigger*. Repeated as a controlled test on the other demo (FirebaseTest,
// a 3.2 MB apk, four interleaved pairs with only the bytes of an asset changing so that each
// pair realigns and resigns): every pair was slower with the flag, mean 4.378s against 4.079s,
// and the package came out 3969 bytes bigger there too. So it loses at both sizes. Skipping
// the padding would also take away the 4K file alignment apksigner adds, which is what lets
// the runtime map an uncompressed entry directly, so the default stands.
func signAPK(keyStore, storePass, keyAlias string) {
	defer timeIt("apksigner")()
	LogI("build", "signing app")

	cmd := exec.Command(apksignerPath, "sign", "--ks-pass", "pass:"+storePass, "--ks", keyStore, "--ks-key-alias", keyAlias, filepath.Join("build", "app.apk"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", toolFailure("apksigner", out, err), apksignerPath)
	}

	// verify with: zipalign -c -v 4 build/app.apk and apksigner verify build/app.apk
}
