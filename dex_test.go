package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestD8ArgsKnowTheApiAndTheCoreLibrary pins the two flags that decide what every
// d8 call produces. Without --min-api d8 assumes API 1 and rewrites every lambda
// of every dependency; --core-library is off unless asked, because with it on d8
// stops reporting a dependency that collides with the platform packages.
func TestD8ArgsKnowTheApiAndTheCoreLibrary(t *testing.T) {
	bare := strings.Join(d8Args(0, false), " ")
	if strings.Contains(bare, "--min-api") || strings.Contains(bare, "--core-library") {
		t.Errorf("an api level of 0 and no core library must add nothing to --lib and --release: %v", bare)
	}

	withAPI := strings.Join(d8Args(26, false), " ")
	if !strings.Contains(withAPI, "--min-api 26") {
		t.Errorf("the api level of the manifest must reach d8: %v", withAPI)
	}

	both := strings.Join(d8Args(21, true), " ")
	if !strings.Contains(both, "--core-library") {
		t.Errorf("--core-library must reach the call: %v", both)
	}
	if !strings.Contains(both, "--min-api 21") {
		t.Errorf("the two flags are independent of each other: %v", both)
	}
	if strings.Contains(strings.Join(d8Args(21, false), " "), "--core-library") {
		t.Error("core library desugaring is opt-in, not a default")
	}
}

// TestDexEntryNamesNumberTheApkDexes: ART opens classes.dex and then classes2.dex
// and on. A name outside that series is an entry the runtime never loads, which is
// how a dependency ends up compiled, dexed, and missing from the installed app.
func TestDexEntryNamesNumberTheApkDexes(t *testing.T) {
	want := []string{"classes.dex", "classes2.dex", "classes3.dex", "classes4.dex"}
	for i, expected := range want {
		if got := dexEntryName(i + 1); got != expected {
			t.Errorf("dexEntryName(%d) = %q, want %q", i+1, got, expected)
		}
	}

	if got := dexEntryName(0); got != "classes.dex" {
		t.Errorf("an index below the first dex must still name a dex ART opens, got %q", got)
	}
}

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
	// a bucket being dexed right now is the same kind of name as a leftover, and the
	// only difference is age: a build that died an hour ago is garbage, one that
	// started a second ago belongs to the other apkc over this project
	if err := os.MkdirAll(filepath.Join(cache, "writing-999-deps-live"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cache, "writing-999-deps-live", "classes.dex"))
	if err := os.MkdirAll(filepath.Join(cache, "writing-998-deps-dead"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cache, "writing-998-deps-dead", "classes.dex"))
	stale := time.Now().Add(-2 * depsStagingTTL)
	if err := os.Chtimes(filepath.Join(cache, "writing-998-deps-dead"), stale, stale); err != nil {
		t.Fatal(err)
	}

	pruneDepsCache()

	if !exists(filepath.Join(cache, "writing-999-deps-live")) {
		t.Fatal("a directory another build could still be dexing into was taken away from it")
	}
	if exists(filepath.Join(cache, "writing-998-deps-dead")) {
		t.Fatal("a staging directory older than anything could still be writing survives forever")
	}
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

// TestDepWorkersNeverStartsMoreJVMsThanTheBuildCanAfford pins the three bounds of the
// pool: one worker per bucket at most, the hard ceiling, and the operator's override.
// A worker that does not fit in memory costs the whole build to the out of memory
// killer, so the number is never allowed to come from the core count alone.
func TestDepWorkersNeverStartsMoreJVMsThanTheBuildCanAfford(t *testing.T) {
	t.Setenv("APKC_JOBS", "")
	if got := depWorkers(0, 0); got != 1 {
		t.Errorf("a build with no bucket still has to be able to dex one: %d", got)
	}
	for _, buckets := range []int{1, 2, 6, 40} {
		got := depWorkers(buckets, 0)
		if got < 1 {
			t.Errorf("%d buckets answered %d workers", buckets, got)
		}
		if got > buckets {
			t.Errorf("%d buckets answered %d workers, more than there is work for", buckets, got)
		}
		if got > depsWorkersMax {
			t.Errorf("%d buckets answered %d workers, over the ceiling of %d", buckets, got, depsWorkersMax)
		}
	}

	t.Setenv("APKC_JOBS", "2")
	if got := depWorkers(6, 0); got != 2 {
		t.Errorf("APKC_JOBS is the way out of a machine that cannot take four: want 2, got %d", got)
	}
	if got := depWorkers(1, 0); got != 1 {
		t.Errorf("an override does not invent workers: want 1, got %d", got)
	}
	t.Setenv("APKC_JOBS", "99")
	if got := depWorkers(3, 0); got != 3 {
		t.Errorf("an override above the bucket count is the bucket count: want 3, got %d", got)
	}
	t.Setenv("APKC_JOBS", "not a number")
	if got := depWorkers(3, 0); got < 1 || got > depsWorkersMax {
		t.Errorf("a garbage override has to fall back to the memory rule, got %d", got)
	}
}

// TestDepWorkersCostIsTheProjectsNotTheTools is the reason the budget is not a constant:
// the two shapes this device was measured over need a different answer from the same
// machine. A bucket of toy jars sits at 86 MB of tree, a bucket of androidx at ~390 MB,
// and one JVM of the latter was seen holding 750 MB alone.
func TestDepWorkersCostIsTheProjectsNotTheTools(t *testing.T) {
	t.Setenv("APKC_JOBS", "")
	// the sixty synthetic jar demo: six buckets of a few KB, on a device with room
	if got := depWorkersIn(6, 6400, 0); got != depsWorkersMax {
		t.Errorf("toy jars on a roomy device want the ceiling, got %d", got)
	}
	// FirebaseTest on the same device: 1.6 MB per bucket on average, and the model
	// still says four because 6.4 GB covers four of them
	if got := depWorkersIn(6, 6400, 2); got != depsWorkersMax {
		t.Errorf("the measured firebase set has room for the ceiling, got %d", got)
	}
	// the same firebase set on a phone with 1.5 GB left: a worker costs 120 + 200*2 MB,
	// so two of them fit and the third is the out of memory killer's answer. The rule
	// is measured: two concurrent d8 over this set were seen at 1.24 GB of tree
	if got := depWorkersIn(6, 1500, 2); got != 2 {
		t.Errorf("want two workers over 1.5 GB with 2 MB buckets, got %d", got)
	}
	// and a set of jars five times bigger does not fit two at all: one JVM of that
	// shape was measured holding 750 MB on its own
	if got := depWorkersIn(6, 700, 10); got != 1 {
		t.Errorf("want one worker when a bucket weighs 10 MB, got %d", got)
	}
	// a machine reporting nothing (no /proc/meminfo) is not a machine with zero memory
	if got := depWorkersIn(6, 0, 5); got != depsWorkersMax {
		t.Errorf("an unknown memory must not stop the pool, got %d", got)
	}
}

// TestBucketOfKeepsJarsWhereTheyAlreadyAre is the property the whole cache of buckets
// is worth: an add re-dexes one bucket only because the jars that were already there
// did not move. Nothing but the count of buckets can move them, and that count comes
// from thresholds rather than from the size of the set.
func TestBucketOfKeepsJarsWhereTheyAlreadyAre(t *testing.T) {
	jars := []string{"jar/a.jar", "jar/b.jar", "jar/firebase-analytics.jar", "jar/junit.jar"}
	before := map[string]int{}
	for _, jar := range jars {
		before[jar] = bucketOf(jar, depBuckets(len(jars)))
	}

	// ten jars per bucket: the set crosses to a new count of buckets at eleven, and
	// until then a dependency added beside these four changes nothing about them
	for i := 0; i < 6; i++ {
		jars = append(jars, filepath.Join("jar", "extra"+string(rune('a'+i))+".jar"))
	}
	if depBuckets(10) != depBuckets(4) {
		t.Fatalf("the thresholds moved between four and ten jars: %d and %d", depBuckets(4), depBuckets(10))
	}
	for jar, bucket := range before {
		if got := bucketOf(jar, depBuckets(len(jars))); got != bucket {
			t.Errorf("%s moved from bucket %d to %d when nothing but a new dependency changed", jar, bucket, got)
		}
	}

	// and every jar lands in a bucket that exists: one that folded outside 0..buckets
	// would be left out of the apk without a word
	for _, bucket := range []int{1, 3, depsBucketsMax} {
		for _, jar := range jars {
			got := bucketOf(jar, bucket)
			if got < 0 || got >= bucket {
				t.Errorf("%s in bucket %d of %d", jar, got, bucket)
			}
		}
	}
}
