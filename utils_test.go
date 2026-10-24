package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
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

func seedTable(t *testing.T, buildType string, timestamps map[string]int64) {
	t.Helper()

	DataTable = Data{Type: buildType, Timestamps: timestamps}
	b, err := json.Marshal(DataTable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DataFile, b, 0644); err != nil {
		t.Fatal(err)
	}
}

func modTimeOf(t *testing.T, path string) int64 {
	t.Helper()

	ns, ok := modTime(path)
	if !ok {
		t.Fatalf("modTime(%s) reported missing file", path)
	}
	return ns
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
	seedTable(t, "apk", map[string]int64{})

	if !setCompilationType("aab") {
		t.Fatal("apk -> aab must report a type change")
	}
	if setCompilationType("aab") {
		t.Fatal("aab -> aab must not report a type change")
	}
}

func TestGetFilesSplitsChanged(t *testing.T) {
	setupProject(t)
	writeFile(t, filepath.Join("res", "layout", "main.xml"))
	writeFile(t, filepath.Join("res", "values", "styles.xml"))

	main := filepath.Join("res", "layout", "main.xml")
	styles := filepath.Join("res", "values", "styles.xml")
	seedTable(t, "apk", map[string]int64{main: 1, styles: modTimeOf(t, styles)})

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
	seedTable(t, "apk", map[string]int64{jar: modTimeOf(t, jar)})

	if changed, _ := getFiles("jar", "jar", true); len(changed) != 0 {
		t.Fatalf("unchanged jar must not be reported as changed: %v", changed)
	}
	changed, _ := getFiles("jar", "jar", false)
	if !equalStrings(changed, []string{jar}) {
		t.Fatalf("classpath must contain every jar, got %v", changed)
	}
}

func TestHasChangedMissingFile(t *testing.T) {
	setupProject(t)
	seedTable(t, "apk", map[string]int64{})

	if hasChanged(filepath.Join("res", "layout", "missing.xml")) {
		t.Fatal("a file without record and without content is not a change")
	}
}

func TestUpdateRecordsIgnoresMissingFiles(t *testing.T) {
	setupProject(t)
	writeFile(t, filepath.Join("src", "demo", "Foo.java"))

	updateRecords([]string{filepath.Join("src", "demo", "Foo.java"), filepath.Join("src", "demo", "Gone.java")})

	if _, ok := DataTable.Timestamps[filepath.Join("src", "demo", "Foo.java")]; !ok {
		t.Fatal("existing file must be recorded")
	}
	if _, ok := DataTable.Timestamps[filepath.Join("src", "demo", "Gone.java")]; ok {
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
	seedTable(t, "apk", map[string]int64{
		main:   modTimeOf(t, main),
		styles: modTimeOf(t, styles),
	})

	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}

	resStale, classStale := pruneDeleted()
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
	if got := DataTable.Timestamps[styles]; got != 0 {
		t.Errorf("living resource must be marked for recompilation, record = %d", got)
	}
	if _, ok := DataTable.Timestamps[main]; ok {
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
	seedTable(t, "apk", map[string]int64{
		foo:     modTimeOf(t, foo),
		bar:     modTimeOf(t, bar),
		homonym: modTimeOf(t, homonym),
	})

	if err := os.Remove(foo); err != nil {
		t.Fatal(err)
	}

	resStale, classStale := pruneDeleted()
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
	if _, ok := DataTable.Timestamps[foo]; ok {
		t.Error("record of the deleted source must be dropped")
	}
}
