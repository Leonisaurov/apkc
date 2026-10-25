package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// setupProject creates an isolated project dir, chdirs into it and resets the
// process-wide incremental state, because DataFile is relative to the cwd.
func setupProject(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	DataTable = Data{}
	t.Cleanup(func() { DataTable = Data{} })
}

func writeFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

// seedTable lays the state open the way the build before this one left it: the
// type it compiled, and for every tracked file the content it compiled from.
func seedTable(t *testing.T, buildType string, records map[string]string) {
	t.Helper()

	DataTable = Data{Version: stateVersion, Type: buildType, Digests: records}
	b, err := json.Marshal(DataTable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DataFile, b, 0644); err != nil {
		t.Fatal(err)
	}
}

// recorded is the entry a file needs in the state to count as already compiled:
// its own bytes. A test that wants "the content moved" writes the file again
// without touching this, and a test that wants "the timestamp moved, the content
// did not" records this and then sets an older mtime.
func recorded(t *testing.T, path string) string {
	t.Helper()

	digest, err := fileSHA1(path)
	if err != nil {
		t.Fatalf("fileSHA1(%s): %v", path, err)
	}
	return digest
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSetCompilationTypeFirstRun(t *testing.T) {
	setupProject(t)

	if setCompilationType("apk") {
		t.Fatal("first run must not report a type change")
	}
	if DataTable.Type != "apk" {
		t.Fatalf("type = %q, want apk", DataTable.Type)
	}
	if !exists(DataFile) {
		t.Fatal("first run must persist the build type")
	}
}

func TestSetCompilationTypeChange(t *testing.T) {
	setupProject(t)
	seedTable(t, "apk", map[string]string{})

	if !setCompilationType("aab") {
		t.Fatal("apk -> aab must report a type change")
	}
	if setCompilationType("aab") {
		t.Fatal("aab -> aab must not report a type change")
	}
}

// TestLoadTableDropsAStateOfAnotherVersion is what makes the format change of
// this file safe: a state whose records answer another question cannot be read as
// an answer, and one round that produces everything is cheaper than an apk that
// decides it has nothing to do.
func TestLoadTableDropsAStateOfAnotherVersion(t *testing.T) {
	setupProject(t)

	body := `{"version":` + strconv.Itoa(stateVersion-1) +
		`,"type":"apk","digests":{"src/com/myapp/Main.java":"aaaa"},"manifestDigest":"bbbb","bundleSha1":"cccc",` +
		`"classApi":{"demo.Main":"dddd"}}`
	if err := os.WriteFile(DataFile, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	loadTable()
	if DataTable.Version != stateVersion {
		t.Fatalf("version = %d, want this build's %d", DataTable.Version, stateVersion)
	}
	if len(DataTable.Digests) != 0 {
		t.Errorf("records of another version must not be trusted, got %v", DataTable.Digests)
	}
	if len(DataTable.ClassAPI) != 0 {
		t.Errorf("the class shapes of another build are not this build's index, got %v", DataTable.ClassAPI)
	}
	if DataTable.ManifestDigest != "" || DataTable.BundleSha1 != "" {
		t.Error("the digests that gate the link and the signature must go with the records")
	}
	// the build type is not a record of staleness: the project did not change
	// shape, it only has to be built again
	if DataTable.Type != "apk" {
		t.Errorf("type = %q, want apk kept", DataTable.Type)
	}
	if setCompilationType("apk") {
		t.Error("a state only upgraded in version must not report a change of shape")
	}
}

func TestSaveTableCarriesTheVersion(t *testing.T) {
	setupProject(t)
	seedTable(t, "apk", map[string]string{})

	b, err := os.ReadFile(DataFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version":`+strconv.Itoa(stateVersion)) {
		t.Fatalf("the state must name the shape it is, got %s", b)
	}

	// and a state this build wrote is read whole, not dropped
	DataTable = Data{}
	loadTable()
	if DataTable.Version != stateVersion || len(DataTable.Digests) != 0 || DataTable.Digests == nil {
		t.Fatalf("a state of this version must load as it was written: %+v", DataTable)
	}
}

func TestGetFilesSplitsChanged(t *testing.T) {
	setupProject(t)
	writeFile(t, filepath.Join("res", "layout", "main.xml"))
	writeFile(t, filepath.Join("res", "values", "styles.xml"))

	main := filepath.Join("res", "layout", "main.xml")
	styles := filepath.Join("res", "values", "styles.xml")
	seedTable(t, "apk", map[string]string{main: "no record of this content", styles: recorded(t, styles)})

	changed, unchanged := getFiles("res", "", true)
	if !equalStrings(changed, []string{main}) {
		t.Errorf("changed = %v, want [%v]", changed, main)
	}
	if !equalStrings(unchanged, []string{styles}) {
		t.Errorf("unchanged = %v, want [%v]", unchanged, styles)
	}

	changed, unchanged = getFiles("res", "", false)
	if len(unchanged) != 0 {
		t.Errorf("non incremental run must not report unchanged files: %v", unchanged)
	}
	if len(changed) != 2 {
		t.Errorf("non incremental run must report every file, got %v", changed)
	}
}

// The jar set feeds -classpath, so callers must ask for the complete list: an
// incremental lookup returns only the jars that changed.
func TestGetFilesJarExtensionFullSet(t *testing.T) {
	setupProject(t)
	jar := filepath.Join("jar", "dep.jar")
	writeFile(t, jar)
	seedTable(t, "apk", map[string]string{jar: recorded(t, jar)})

	if changed, _ := getFiles("jar", ".jar", true); len(changed) != 0 {
		t.Fatalf("unchanged jar must not be reported as changed: %v", changed)
	}
	changed, _ := getFiles("jar", ".jar", false)
	if !equalStrings(changed, []string{jar}) {
		t.Fatalf("classpath must contain every jar, got %v", changed)
	}
}

func TestHasChangedMissingFile(t *testing.T) {
	setupProject(t)
	seedTable(t, "apk", map[string]string{})

	if hasChanged(filepath.Join("res", "layout", "missing.xml")) {
		t.Fatal("a file without record and without content is not a change")
	}
}

// TestHasChangedFollowsBytesNotTimestamps is the reason the records are digests.
// A `cp -p`, a tar restore or a sync puts new bytes under the timestamp the state
// remembers, and by mtime that is not a change: the project keeps compiling
// against a class nobody rebuilt, and the apk ships it.
func TestHasChangedFollowsBytesNotTimestamps(t *testing.T) {
	setupProject(t)
	src := filepath.Join("src", "demo", "Foo.java")
	writeFile(t, src)
	seedTable(t, "apk", map[string]string{src: recorded(t, src)})

	if hasChanged(src) {
		t.Fatal("a file nobody touched must not be compiled again")
	}

	stamp := statModTime(t, src)
	if err := os.WriteFile(src, []byte("class Foo { int different; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if !hasChanged(src) {
		t.Fatal("new bytes under the timestamp the state remembers must be compiled again")
	}

	// and the other direction: the same bytes under a timestamp from the future
	// are still the same bytes, and must not cost a compilation
	if err := os.WriteFile(src, []byte("class Foo { int different; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	future := stamp.Add(48 * time.Hour)
	if err := os.Chtimes(src, future, future); err != nil {
		t.Fatal(err)
	}
	updateRecords([]string{src})
	if hasChanged(src) {
		t.Fatal("a rewrite of recorded content must stay unchanged")
	}
}

func statModTime(t *testing.T, path string) time.Time {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func TestUpdateRecordsIgnoresMissingFiles(t *testing.T) {
	setupProject(t)
	writeFile(t, filepath.Join("src", "demo", "Foo.java"))

	updateRecords([]string{filepath.Join("src", "demo", "Foo.java"), filepath.Join("src", "demo", "Gone.java")})

	if _, ok := DataTable.Digests[filepath.Join("src", "demo", "Foo.java")]; !ok {
		t.Fatal("existing file must be recorded")
	}
	if _, ok := DataTable.Digests[filepath.Join("src", "demo", "Gone.java")]; ok {
		t.Fatal("missing file must not be recorded")
	}
}

func TestPruneDeletedPurgesFlatsAndForcesRecompile(t *testing.T) {
	setupProject(t)
	main := filepath.Join("res", "layout", "main.xml")
	styles := filepath.Join("res", "values", "styles.xml")
	writeFile(t, main)
	writeFile(t, styles)

	// aapt2 names a flat after the resource path, not after its base name
	flatDir := filepath.Join("build", "flats")
	writeFile(t, filepath.Join(flatDir, "layout_main.xml.flat"))
	writeFile(t, filepath.Join(flatDir, "values_styles.arsc.flat"))
	seedTable(t, "apk", map[string]string{
		main:   recorded(t, main),
		styles: recorded(t, styles),
	})

	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}

	resStale, classStale := pruneDeleted(true)
	if !resStale {
		t.Error("deleted resource must invalidate the linked resources")
	}
	if classStale {
		t.Error("deleted resource must not invalidate compiled classes")
	}

	flats, err := filepath.Glob(filepath.Join(flatDir, "*.flat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(flats) != 0 {
		t.Errorf("every flat must be purged so no deleted resource survives, got %v", flats)
	}
	if _, ok := DataTable.Digests[styles]; ok {
		t.Error("a living resource must be marked for recompilation by dropping its record")
	}
	if _, ok := DataTable.Digests[main]; ok {
		t.Error("record of a deleted file must be dropped")
	}
}

func TestPruneDeletedRemovesInnerClasses(t *testing.T) {
	setupProject(t)
	foo := filepath.Join("src", "demo", "Foo.java")
	bar := filepath.Join("src", "demo", "Bar.java")
	homonym := filepath.Join("src", "other", "Foo.java")
	writeFile(t, foo)
	writeFile(t, bar)
	writeFile(t, homonym)

	classDir := filepath.Join("build", "classes", "demo")
	writeFile(t, filepath.Join(classDir, "Foo.class"))
	writeFile(t, filepath.Join(classDir, "Foo$1.class"))
	writeFile(t, filepath.Join(classDir, "Bar.class"))
	// same class name in another package must survive
	writeFile(t, filepath.Join("build", "classes", "other", "Foo.class"))
	seedTable(t, "apk", map[string]string{
		foo:     recorded(t, foo),
		bar:     recorded(t, bar),
		homonym: recorded(t, homonym),
	})

	if err := os.Remove(foo); err != nil {
		t.Fatal(err)
	}

	resStale, classStale := pruneDeleted(true)
	if resStale {
		t.Error("deleted source must not invalidate resources")
	}
	if !classStale {
		t.Error("deleted source must invalidate the dexed classes")
	}

	var left []string
	filepath.Walk(filepath.Join("build", "classes"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, rerr := filepath.Rel(filepath.Join("build", "classes"), path)
			if rerr != nil {
				t.Error(rerr)
			}
			left = append(left, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(left)
	want := []string{
		filepath.ToSlash(filepath.Join("demo", "Bar.class")),
		filepath.ToSlash(filepath.Join("other", "Foo.class")),
	}
	sort.Strings(want)
	if !equalStrings(left, want) {
		t.Errorf("classes left in build/classes = %v, want %v", left, want)
	}
	if _, ok := DataTable.Digests[foo]; ok {
		t.Error("record of the deleted source must be dropped")
	}
}

// A dependency is not an input of the dex of the project above API 21, so losing one has to
// leave the sources alone: forcing them through d8 for a jar the bundle drops on its own is
// the cost the split exists to remove. Below API 21 everything shares one merge, and there a
// deleted jar really does stale the dex.
func TestPruneDeletedJarFollowsTheDexLayout(t *testing.T) {
	cases := []struct {
		name       string
		depsMerged bool
		wantClass  bool
	}{
		{"api 21", false, false},
		{"api 19", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupProject(t)
			jarFile := filepath.Join("jar", "dep-1.0.jar")
			writeFile(t, jarFile)
			seedTable(t, "apk", map[string]string{jarFile: recorded(t, jarFile)})

			if err := os.Remove(jarFile); err != nil {
				t.Fatal(err)
			}
			resStale, classStale := pruneDeleted(tc.depsMerged)

			if resStale {
				t.Error("a deleted jar never invalidates the linked resources")
			}
			if classStale != tc.wantClass {
				t.Errorf("classStale = %v, want %v", classStale, tc.wantClass)
			}
			if _, ok := DataTable.Digests[jarFile]; ok {
				t.Error("record of the deleted jar must be dropped")
			}
		})
	}
}

func TestKotlinShortcomingsNamesOnlyWhatIsActuallyMissing(t *testing.T) {
	// os.Args[0] is the test binary: a real executable, so "compiler present" is
	// answered without depending on what this machine happens to have installed
	present := os.Args[0]
	absent := "apkc-no-such-compiler"

	if got := kotlinShortcomings(present, []string{"jar/gson-2.10.1.jar", "jar/kotlin-stdlib-2.0.21.jar"}); len(got) != 0 {
		t.Errorf("a project that has both was reported as lacking %v", got)
	}

	got := kotlinShortcomings(present, []string{"jar/gson-2.10.1.jar"})
	if len(got) != 1 || !strings.Contains(got[0], "kotlin-stdlib") {
		t.Errorf("the missing stdlib was reported as %v", got)
	}

	got = kotlinShortcomings(absent, []string{"jar/kotlin-stdlib-2.0.21.jar"})
	if len(got) != 1 || !strings.Contains(got[0], "PATH") {
		t.Errorf("the missing compiler was reported as %v", got)
	}

	got = kotlinShortcomings(absent, nil)
	if len(got) != 2 {
		t.Errorf("a project missing both was reported as %v, want two names", got)
	}
}

func TestClasspathUsesThePlatformSeparator(t *testing.T) {
	entries := []string{"android.jar", "build/classes", "jar/a.jar", "jar/b.jar"}
	want := strings.Join(entries, string(filepath.ListSeparator))
	if got := classpath(entries); got != want {
		t.Errorf("classpath = %q, want %q", got, want)
	}

	if got := classpath([]string{"only"}); got != "only" {
		t.Errorf("one entry must carry no separator, got %q", got)
	}

	// every jar of the project has to survive the join: a classpath that drops a
	// dependency compiles and then crashes at runtime
	jars := []string{"jar/a.jar", "jar/b.jar"}
	joined := classpath(append([]string{"android.jar"}, jars...))
	for _, jar := range jars {
		if !strings.Contains(joined, jar) {
			t.Errorf("%s is not in the classpath %q", jar, joined)
		}
	}
	if strings.Contains(joined, "android.jar"+string(filepath.ListSeparator)+"android.jar") {
		t.Error("android.jar must be listed once")
	}
}
