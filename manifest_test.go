package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeManifest(t *testing.T, body string) {
	t.Helper()

	if err := os.WriteFile("AndroidManifest.xml", []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestMinAPI(t *testing.T) {
	setupProject(t)

	writeManifest(t, `<manifest package="com.myapp">
	<uses-sdk android:minSdkVersion="26" android:targetSdkVersion="33"/>
	<application/></manifest>`)
	if got := manifestMinAPI(); got != 26 {
		t.Fatalf("minSdkVersion 26 read as %d", got)
	}

	// a manifest without uses-sdk must not invent a number: d8 has to keep the
	// behaviour it had before the flag existed
	writeManifest(t, `<manifest package="com.myapp"><application/></manifest>`)
	if got := manifestMinAPI(); got != 0 {
		t.Fatalf("missing uses-sdk read as %d, want 0", got)
	}

	writeManifest(t, `<manifest package="com.myapp"><uses-sdk android:minSdkVersion="abc"/><application/></manifest>`)
	if got := manifestMinAPI(); got != 0 {
		t.Fatalf("unparseable minSdkVersion read as %d, want 0", got)
	}

	if err := os.Remove("AndroidManifest.xml"); err != nil {
		t.Fatal(err)
	}
	if got := manifestMinAPI(); got != 0 {
		t.Fatalf("absent manifest read as %d, want 0", got)
	}
}

// writeGenJava lays an R.java down with an old mtime, the way a previous build
// would have left it in the incremental records.
func writeGenJava(t *testing.T, path string, content string) time.Time {
	t.Helper()

	old := time.Now().Add(-48 * time.Hour).Round(time.Second)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return old
}

func TestSettleGenJavaKeepsUnchangedFileAsUnchanged(t *testing.T) {
	setupProject(t)

	const body = "package com.myapp;\npublic final class R { int a=1; }\n"
	path := filepath.Join("src", "com", "myapp", "R.java")
	old := writeGenJava(t, path, body)
	// the record a previous build left behind: without it every file looks new
	seedTable(t, "apk", map[string]int64{path: old.UnixNano()})

	before := snapshotGenJava()
	// aapt2 link rewrites every R.java on every run, even with the same ids
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if !hasChanged(path) {
		t.Fatal("rewriting the file must look like a change before settling")
	}

	if settleGenJava(before) {
		t.Fatal("identical content must not report a change")
	}
	if hasChanged(path) {
		t.Fatal("unchanged content must keep the mtime it was snapshotted with")
	}
	if got := modTimeOf(t, path); got != old.UnixNano() {
		t.Fatalf("mtime restored to %d, want %d", got, old.UnixNano())
	}
}

func TestSettleGenJavaReportsChangedContent(t *testing.T) {
	setupProject(t)

	path := filepath.Join("src", "com", "myapp", "R.java")
	old := writeGenJava(t, path, "package com.myapp;\npublic final class R { int a=1; }\n")
	seedTable(t, "apk", map[string]int64{path: old.UnixNano()})

	before := snapshotGenJava()
	if err := os.WriteFile(path, []byte("package com.myapp;\npublic final class R { int a=2; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if !settleGenJava(before) {
		t.Fatal("different content must report a change")
	}
	if !hasChanged(path) {
		t.Fatal("changed content must keep its new mtime")
	}
}

func TestSettleGenJavaReportsNewAndVanishedFiles(t *testing.T) {
	setupProject(t)

	kept := filepath.Join("src", "com", "myapp", "R.java")
	const body = "package com.myapp;\npublic final class R { int a=1; }\n"
	writeGenJava(t, kept, body)

	before := snapshotGenJava()

	// a new resource package adds an R.java
	fresh := filepath.Join("src", "com", "myapp", "extra", "R.java")
	if err := os.MkdirAll(filepath.Dir(fresh), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if !settleGenJava(before) {
		t.Fatal("a new R.java must report a change")
	}

	// and a package whose R.java disappeared is a change too
	before = snapshotGenJava()
	if err := os.Remove(fresh); err != nil {
		t.Fatal(err)
	}
	if !settleGenJava(before) {
		t.Fatal("a vanished R.java must report a change")
	}
}

// Manifest.java is the other file aapt writes into src/ (the old aapt artifact for
// the <permission> elements of the merged manifest). aapt2 rewrites it on every
// link, so leaving it out of the comparison made a resource edit recompile every
// source of the project: measured as 59s instead of 10s on a 60 jar project.
func TestSettleGenJavaCoversManifestJava(t *testing.T) {
	setupProject(t)

	const body = "package com.myapp;\npublic final class Manifest { public static final class permission { public static final String P=\"x\"; } }\n"
	path := filepath.Join("src", "com", "myapp", "Manifest.java")
	old := writeGenJava(t, path, body)
	seedTable(t, "apk", map[string]int64{path: old.UnixNano()})

	before := snapshotGenJava()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if !hasChanged(path) {
		t.Fatal("rewriting the file must look like a change before settling")
	}
	if settleGenJava(before) {
		t.Fatal("an unchanged Manifest.java must not invalidate the sources")
	}
	if hasChanged(path) {
		t.Fatal("an unchanged Manifest.java must keep its mtime")
	}

	// and the same file with a real change has to report it
	before = snapshotGenJava()
	if err := os.WriteFile(path, []byte(strings.Replace(body, `P="x"`, `P="y"`, 1)), 0644); err != nil {
		t.Fatal(err)
	}
	if !settleGenJava(before) {
		t.Fatal("a Manifest.java whose permission changed must invalidate the sources")
	}
}

// The fingerprint is what decides whether the dependencies changed, so it has to
// move when the bytes move and stay when they do not, api level included.
func TestJarsFingerprintFollowsContent(t *testing.T) {
	digests := []string{"aaa", "bbb"}

	same := jarsFingerprint(26, digests)
	if again := jarsFingerprint(26, []string{"aaa", "bbb"}); again != same {
		t.Fatalf("same content produced a different fingerprint: %s vs %s", again, same)
	}
	if other := jarsFingerprint(26, []string{"aaa", "ccc"}); other == same {
		t.Fatal("a changed jar must change the fingerprint")
	}
	if other := jarsFingerprint(26, []string{"bbb", "aaa"}); other == same {
		t.Fatal("jar order is part of what the merge consumed")
	}
	if other := jarsFingerprint(24, digests); other == same {
		t.Fatal("a different api level desugars differently, it cannot share a fingerprint")
	}
	if empty := jarsFingerprint(26, []string{}); empty == "" {
		t.Fatal("a project without jars still needs a fingerprint, the empty one means never merged")
	}
}

func TestFileSHA1KeysByContent(t *testing.T) {
	setupProject(t)

	a := filepath.Join("jar", "one.jar")
	b := filepath.Join("jar", "two.jar")
	writeFile(t, a)
	writeFile(t, b)

	first, err := fileSHA1(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fileSHA1(b)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same bytes produced different digests: %s vs %s", first, second)
	}

	if err := os.WriteFile(b, []byte("y\n"), 0644); err != nil {
		t.Fatal(err)
	}
	third, err := fileSHA1(b)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("different bytes produced the same digest")
	}

	if _, err := fileSHA1(filepath.Join("jar", "missing.jar")); err == nil {
		t.Fatal("fileSHA1 must report an error for a missing file")
	}
}

func TestManifestChangedFollowsContent(t *testing.T) {
	setupProject(t)

	writeManifest(t, "<manifest package=\"a\"/>\n")
	if !manifestChanged() {
		t.Fatal("a manifest the state never read must report a change")
	}
	if manifestChanged() {
		t.Fatal("the same bytes must not report a change twice")
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes("AndroidManifest.xml", later, later); err != nil {
		t.Fatal(err)
	}
	if manifestChanged() {
		t.Fatal("a fresh timestamp over the same bytes is not a manifest change")
	}

	writeManifest(t, "<manifest package=\"b\"/>\n")
	if !manifestChanged() {
		t.Fatal("different bytes must report a change")
	}

	// a missing manifest cannot be compared, so it has to count as a change and
	// let the build fail over it instead of proceeding with stale artifacts
	if err := os.Remove("AndroidManifest.xml"); err != nil {
		t.Fatal(err)
	}
	if !manifestChanged() {
		t.Fatal("a missing manifest must report a change")
	}
}

// rosterText is the roster alone: the tests of the incremental decision do not care
// about the per file digests the bundle reuses.
func rosterText() string {
	roster, _ := auxRoster(false)
	return roster
}

func TestAuxRosterDigestsNameTheFiles(t *testing.T) {
	setupProject(t)

	if err := os.MkdirAll("assets", 0755); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join("assets", "dato.txt")
	if err := os.WriteFile(asset, []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}

	_, digests := auxRoster(false)
	want, err := fileSHA1(asset)
	if err != nil {
		t.Fatal(err)
	}
	if got := digests[asset]; got != want {
		t.Fatalf("digest of %s = %q, want %q", asset, got, want)
	}
	if len(digests) != 1 {
		t.Fatalf("digests = %d entries, want the one asset", len(digests))
	}

	// the same name with other bytes must not keep the digest that was recorded for
	// it, or the bundle would copy a stale entry raw
	if err := os.WriteFile(asset, []byte("two\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, after := auxRoster(false); after[asset] == want {
		t.Fatal("a rewritten asset must report a new digest")
	}
}

func TestAuxRosterFollowsNamesAndBytes(t *testing.T) {
	setupProject(t)

	empty := rosterText()

	if err := os.MkdirAll(filepath.Join("lib", "arm64-v8a"), 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join("lib", "arm64-v8a", "libfoo.so")
	if err := os.WriteFile(lib, []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	one := rosterText()
	if one == empty {
		t.Fatal("a native library added to an empty project must change the roster")
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(lib, later, later); err != nil {
		t.Fatal(err)
	}
	if rosterText() != one {
		t.Fatal("the same bytes under a new timestamp must not change the roster")
	}

	if err := os.WriteFile(lib, []byte("two\n"), 0644); err != nil {
		t.Fatal(err)
	}
	two := rosterText()
	if two == one {
		t.Fatal("a native library whose bytes changed must change the roster")
	}

	asset := filepath.Join("assets", "dato.txt")
	if err := os.MkdirAll("assets", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	withAsset := rosterText()
	if withAsset == two {
		t.Fatal("an added asset must change the roster")
	}

	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	if rosterText() == withAsset {
		t.Fatal("a deleted asset must change the roster")
	}
	if rosterText() != two {
		t.Fatal("the roster must come back to the state without the asset")
	}
}

// TestAuxRosterFollowsThePackingOfNativeLibs covers the case a manifest edit
// alone produces: the .so bytes are untouched, only the method they travel with
// changes, and a roster that misses it leaves the old deflated entry in the apk.
func TestAuxRosterFollowsThePackingOfNativeLibs(t *testing.T) {
	setupProject(t)

	if err := os.MkdirAll(filepath.Join("lib", "arm64-v8a"), 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join("lib", "arm64-v8a", "libfoo.so")
	if err := os.WriteFile(lib, []byte("same bytes\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deflated := rosterText()
	stored, _ := auxRoster(true)
	if stored == deflated {
		t.Fatal("extractNativeLibs=false packs the same file another way, and the roster has to say so")
	}
	if again, _ := auxRoster(true); again != stored {
		t.Fatal("the same decision must answer the same roster")
	}

	// assets are not native libraries: the attribute does not reach them
	if err := os.MkdirAll("assets", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("assets", "dato.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	withAssetDeflated := rosterText()
	withAssetStored, _ := auxRoster(true)
	if withAssetStored == withAssetDeflated {
		t.Fatal("the asset must not hide the difference lib/ makes")
	}
}

func TestManifestNativeLibsStored(t *testing.T) {
	setupProject(t)

	// "false" is the promise that the runtime maps the .so out of the package,
	// which only holds when the entry is stored and page aligned
	writeManifest(t, `<manifest package="com.myapp"><application android:extractNativeLibs="false"/></manifest>`)
	if stored, declared := manifestNativeLibsStored(); stored != true || declared != true {
		t.Fatalf("extractNativeLibs=false read as stored=%v declared=%v", stored, declared)
	}

	writeManifest(t, `<manifest package="com.myapp"><application android:extractNativeLibs="true"/></manifest>`)
	if stored, declared := manifestNativeLibsStored(); stored != false || declared != true {
		t.Fatalf("extractNativeLibs=true read as stored=%v declared=%v", stored, declared)
	}

	// no attribute is not a declaration: apkc deflates, which is what every
	// Android version accepts, and says it unpacked the choice rather than read it
	writeManifest(t, `<manifest package="com.myapp"><application/></manifest>`)
	if stored, declared := manifestNativeLibsStored(); stored != false || declared != false {
		t.Fatalf("no attribute read as stored=%v declared=%v", stored, declared)
	}

	// a reference is not a literal the build can act on
	writeManifest(t, `<manifest package="com.myapp"><application android:extractNativeLibs="@bool/unpacked"/></manifest>`)
	if stored, _ := manifestNativeLibsStored(); stored {
		t.Fatal("a reference must not be read as the stored layout")
	}

	if err := os.Remove("AndroidManifest.xml"); err != nil {
		t.Fatal(err)
	}
	if stored, declared := manifestNativeLibsStored(); stored || declared {
		t.Fatal("an absent manifest decides nothing about lib/")
	}
}
