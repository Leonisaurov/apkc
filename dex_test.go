package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The apk numbers its dex files from one. Getting that numbering wrong is not a
// warning: a collision over classes.dex silently drops whichever dex the bundle
// wrote second.
func TestDexIndexOrdersWhatD8Writes(t *testing.T) {
	setupProject(t)

	dir := filepath.Join("build", "dexcache", "deps-deadbeef")
	for _, name := range []string{"classes10.dex", "classes2.dex", "classes.dex", "notes.txt"} {
		writeFile(t, filepath.Join(dir, name))
	}

	dexes := dexFilesIn(dir)
	want := []string{
		filepath.Join(dir, "classes.dex"),
		filepath.Join(dir, "classes2.dex"),
		filepath.Join(dir, "classes10.dex"),
	}
	if len(dexes) != len(want) {
		t.Fatalf("dexFilesIn returned %v, want %v", dexes, want)
	}
	for i, path := range want {
		if dexes[i] != path {
			t.Fatalf("dex order is by index, not by name: %v", dexes)
		}
	}

	if dexEntryName(1) != "classes.dex" {
		t.Fatalf("the first entry has to be classes.dex, got %s", dexEntryName(1))
	}
	if dexEntryName(2) != "classes2.dex" {
		t.Fatalf("the second entry has to be classes2.dex, got %s", dexEntryName(2))
	}
}

func TestDexIndexKeepsAnUnnumberedFileLast(t *testing.T) {
	if dexIndex("classes.dex") != 1 {
		t.Fatal("classes.dex is the first dex")
	}
	if dexIndex("classes.dex") >= dexIndex("classes2.dex") {
		t.Fatal("classes.dex comes before classes2.dex")
	}
	if dexIndex("classes2.dex") >= dexIndex("classes10.dex") {
		t.Fatal("classes2.dex comes before classes10.dex")
	}
	if dexIndex("classes.dex") >= dexIndex("classesodd.dex") {
		t.Fatal("a name d8 did not write must not push the numbered files around it")
	}
}

func writeJar(t *testing.T, path string, names ...string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	w := zip.NewWriter(file)
	for _, name := range names {
		dst, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dst.Write([]byte("not a class")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// Splitting the dex files removed the only check that caught a class both the
// project and a dependency define: the runtime picks one of them and says nothing.
func TestShadowedClassCatchesWhatTheMergeUsedTo(t *testing.T) {
	setupProject(t)

	writeFile(t, filepath.Join("build", "classes", "com", "myapp", "Dep.class"))
	jar := filepath.Join("jar", "dep-1.0.jar")
	writeJar(t, jar, "com/myapp/Dep.class", "com/myapp/Other.class")

	class, where, dup := shadowedClass([]string{jar})
	if !dup {
		t.Fatal("a class the project compiles and the jar ships must abort the build")
	}
	if class != "com.myapp.Dep in dep-1.0.jar" {
		t.Fatalf("unexpected report: %s", class)
	}
	if where != jar {
		t.Fatalf("the jar has to be named, got %s", where)
	}

	// and a project that shares nothing with its dependencies is the normal case
	if err := os.Remove(filepath.Join("build", "classes", "com", "myapp", "Dep.class")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join("build", "classes", "com", "myapp", "MainActivity.class"))
	if _, _, dup := shadowedClass([]string{jar}); dup {
		t.Fatal("the build must not abort over a class only the dependency holds")
	}
}

func TestShadowedClassIgnoresWhatD8Discards(t *testing.T) {
	setupProject(t)

	writeFile(t, filepath.Join("build", "classes", "com", "myapp", "package-info.class"))
	jar := filepath.Join("jar", "dep-1.0.jar")
	writeJar(t, jar, "com/myapp/package-info.class", "META-INF/versions/9/com/myapp/package-info.class")

	if _, _, dup := shadowedClass([]string{jar}); dup {
		t.Fatal("d8 throws these away, so they are not a collision")
	}
}

func TestJarClassesListsTheClassesOfAJar(t *testing.T) {
	setupProject(t)

	jar := filepath.Join("jar", "dep-1.0.jar")
	writeJar(t, jar, "com/myapp/Dep.class", "com/myapp/notes.txt")

	names, err := jarClasses(jar)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "com/myapp/Dep" {
		t.Fatalf("want the one class of the jar, got %v", names)
	}

	if _, err := jarClasses(filepath.Join("jar", "missing.jar")); err == nil {
		t.Fatal("an unreadable jar has to report it, the caller decides what to do")
	}
}

// The cache is the whole of jar/ dexed, so it grows with every version ever tried.
func TestPruneDepsCacheKeepsTheNewestSets(t *testing.T) {
	setupProject(t)

	cache := filepath.Join("build", "dexcache")
	old := time.Now().Add(-time.Duration(depsCacheKeep) * time.Hour)
	for i := 0; i < depsCacheKeep+2; i++ {
		dir := filepath.Join(cache, "deps-set"+string(rune('a'+i)))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "classes.dex"))
		stamp := old.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(dir, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// a marker of a set still kept is not a set, and the per jar dexes of the layout
	// this replaced are dead weight the first prune has to take away
	if err := os.WriteFile(filepath.Join(cache, "deps-setf.nodex"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "deps-seta.nodex"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cache, "abcdef.dex"))
	writeFile(t, filepath.Join(cache, "abcdef.tmp", "classes.dex"))

	pruneDepsCache()

	if exists(filepath.Join(cache, "abcdef.dex")) || exists(filepath.Join(cache, "abcdef.tmp")) {
		t.Fatal("the dexes of the per jar layout are not read by anybody and have to go")
	}
	if !exists(filepath.Join(cache, "deps-setf.nodex")) {
		t.Fatal("the marker of a kept set has to stay, it is what keeps d8 away from it")
	}
	kept := 0
	for i := 0; i < depsCacheKeep+2; i++ {
		if exists(filepath.Join(cache, "deps-set"+string(rune('a'+i)))) {
			kept++
		}
	}
	if kept != depsCacheKeep {
		t.Fatalf("want %d newest sets, kept %d", depsCacheKeep, kept)
	}
	if exists(filepath.Join(cache, "deps-seta")) {
		t.Fatal("the oldest set survived")
	}
	if exists(filepath.Join(cache, "deps-seta.nodex")) {
		t.Fatal("the marker of a pruned set would keep a forgotten set out of the cache")
	}
	if !exists(filepath.Join(cache, "deps-set"+string(rune('a'+depsCacheKeep+1)))) {
		t.Fatal("the newest set was pruned, it is the one the build just wrote")
	}
}
