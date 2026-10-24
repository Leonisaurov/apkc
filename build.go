package main

import (
	"archive/zip"
	"flag"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// depsBucketsMax caps how far the set of jar/ is split. Each bucket is a d8 call of
// its own, so more than a handful start costing their own start up, and an add only
// needs them small enough that one changed jar moves a fraction of the whole.
const depsBucketsMax = 6

// depsCacheKeep counts bucket directories, not whole sets, because the cache now holds
// jar/ split into buckets. It has to stay above depsBucketsMax so the buckets of the
// roster in use are never the oldest thing in the cache: dexBucket refreshes the mtime
// of every bucket it reads, hit or miss, so "newest" means "used this build" and a
// live project never evicts a directory it is about to reuse.
const depsCacheKeep = 2 * depsBucketsMax

// buildOpts are the options every build stage needs, shared by build, run and watch.
type buildOpts struct {
	useAAB      bool
	incremental bool
	keyStore    string
	storePass   string
	keyAlias    string
	sigAlg      string
}

func build() {
	buildCmd := flag.NewFlagSet("build", flag.ExitOnError)

	useAAB := buildCmd.Bool("aab", false, "build aab instead of apk")
	incremental := buildCmd.Bool("inc", false, "Use the incremental build system.")
	keyStore := buildCmd.String("keystore", keyStorePath, "path to keystore")
	storePass := buildCmd.String("storepass", "android", "keystore password")
	keyAlias := buildCmd.String("keyalias", "androiddebugkey", "key alias to use")
	sigAlg := buildCmd.String("sigalg", "SHA256withRSA", "signature to use")

	buildCmd.Parse(os.Args[2:])
	_build(buildOpts{
		useAAB:      *useAAB,
		incremental: *incremental,
		keyStore:    *keyStore,
		storePass:   *storePass,
		keyAlias:    *keyAlias,
		sigAlg:      *sigAlg,
	})
}

// build compiles all the source code and bundles into apk file with dependencies
func _build(o buildOpts) {
	defer timeIt("total")()
	prepare()

	// d8 decides how much to desugar from this number, and without it it assumes
	// API 1 and rewrites every lambda of every dependency
	api := manifestMinAPI()

	var settedType, resStale, classStale, manifestStale, apiChanged bool
	firstRun := false
	if o.incremental {
		// without any record the whole build dir must be treated as missing,
		// even when every input looks unchanged
		firstRun = !exists(DataFile)
		if o.useAAB {
			settedType = setCompilationType("aab")
		} else {
			settedType = setCompilationType("apk")
		}
		resStale, classStale = pruneDeleted(api < 21)
		// the api level is an input of the project dex: --min-api decides how much
		// of the sources d8 desugars, so a minSdkVersion edit has to dex them again
		// even when no source changed (DataTable is loaded by the calls above)
		apiChanged = api != DataTable.AppApi
	}

	if settedType {
		LogI("Diagnostic", "Detected proyect type changed.", "Rebuilding.")
	}

	// The manifest is an input of the link and of nothing else. `aapt2 compile`
	// turns every resource file into a flat before the package name or a resource
	// id exists, so a new versionName or a declared permission does not
	// invalidate a single flat. The link is still forced, and what it rewrites
	// besides the merged manifest it embeds is R.java and Manifest.java under src/,
	// which settleGenJava compares by content: the sources pay javac only when a
	// constant really changed. Marking every flat and source changed here used to
	// cost a whole compile and javac pass over a `versionName` bump.
	if o.incremental && manifestChanged() {
		manifestStale = true
	}

	// an intermediate deleted by hand must be produced again, no matter what
	// the timestamps say about the inputs
	linkedFile := filepath.Join("build", "resources.arsc")
	if o.useAAB {
		linkedFile = filepath.Join("build", "resources.pb")
	}
	resMissing := !exists(linkedFile)

	// a stage that did its work writes its records down before anything below it can
	// abort. A fatal error exits without ever reaching the save at the end, and a
	// source that compiled but stayed unrecorded leaves its .class in build/ with
	// nothing to prune it once the file is gone: that stale class then aborts every
	// following build. Idle cycles still write nothing.
	compRes := compileRes(o.incremental)
	if o.incremental && compRes {
		saveTable()
	}
	if !o.incremental || firstRun || settedType || compRes || resStale || resMissing || manifestStale {
		// the files aapt generates were rewritten: the sources that read R.*
		// constants have to go through javac again, because javac inlines them
		// into the class files
		if bundleRes(o.useAAB, o.incremental) {
			_, unchanged := getFiles("src", "kt", true)
			markAsChanged(unchanged)
			_, unchanged = getFiles("src", "java", true)
			markAsChanged(unchanged)
		}
	}

	compSource := compileKotlin(o.incremental)
	compSource = compileJava(o.incremental) || compSource
	if o.incremental && compSource {
		saveTable()
	}
	// the compiled classes are listed after the compilers ran, or a source added this
	// cycle would be dexed from the list of the previous one and never reach the apk.
	// The same list is what d8 is handed, so the directory is walked once.
	classes, _ := getFiles(filepath.Join("build", "classes"), ".class", false)
	// a project without sources has no app dex to wait for: bundleJava returns without
	// writing one, so counting it as missing would rebuild, realign and resign the apk
	// on every run of an empty project
	dexMissing := !exists(filepath.Join("build", "classes.dex")) && len(classes) > 0
	// what the dependencies contribute to the dex is decided by their bytes, not
	// by their timestamps: hashing jar/ costs a tenth of a second for 60 jars,
	// and a timestamp cannot tell a sync that rewrote a jar from a new dependency
	jars := allJars()
	digests := jarDigests(jars)
	fingerprint := jarsFingerprint(api, digests)
	// below API 21 the runtime cannot open a second dex file, so there the jars stay
	// inputs of one merge over everything and every edit pays for all of them
	splitDeps := api >= 21
	depDexes := []string{}
	depStale := false
	if splitDeps {
		// the merge was the only thing that caught a class the project and a
		// dependency both define: handed two dex files the runtime picks one of
		// them and no build log ever mentions it
		if class, jar, dup := shadowedClass(jars); dup {
			LogF("build", "Type "+class+" is defined multiple times", "the project and "+jar+" both hold it")
		}
		depDexes, depStale = dexDeps(api, jars, digests)
	}
	// two different decisions that used to be one flag: whether the bundle has to take
	// the dexes of a changed dependency set, and whether the classes of the project
	// have to go through d8 again. Above API 21 the dependencies are not inputs of the
	// project dex, so an add that every bucket cache already answered was costing a
	// whole pass over the sources of the project for nothing
	depChanged := depStale || fingerprint != DataTable.DexInputs
	// below API 21 the jars are inputs of the one merge, so there a dependency does
	// change the dex the project shares with them, and it has to be dexed again
	appStale := !o.incremental || firstRun || settedType || compSource || classStale ||
		apiChanged || (depChanged && !splitDeps) || dexMissing
	if appStale {
		bundleJava(api, jars, splitDeps, classes)
		if o.incremental && len(classes) > 0 {
			// recorded only when a dex was really written: an empty src/ produces
			// nothing to attribute to this api level
			DataTable.AppApi = api
		}
	}

	// the copied trees decide the bundle by their roster, and a bundle that is not
	// rebuilt would keep the .so and assets of whatever was there before
	var auxStale bool
	var auxDigests map[string]string
	roster := ""
	if o.incremental {
		roster, auxDigests = auxRoster()
		auxStale = roster != DataTable.AuxInputs
	}

	needBundle := !o.incremental || firstRun || settedType || compRes || compSource || resStale || classStale ||
		manifestStale || depChanged || auxStale || resMissing || dexMissing || !exists("build/bundle.zip")
	if needBundle {
		buildBundle(o.useAAB, depDexes, auxDigests)
		if o.incremental {
			DataTable.AuxInputs = roster
			// the bundle is what consumes a dependency set: recording the
			// fingerprint with the dex call would claim those bytes reached the apk
			// when the bundle was skipped
			DataTable.DexInputs = fingerprint
		}
	}

	// zipalign and apksigner are two java starts and a whole rewrite of the apk, so
	// under --inc they only run when the bytes they would be handed really changed.
	// A plain build always runs them: a caller of `apkc build` without --inc judges
	// the build by the apk being rewritten, and that signal is not worth losing
	// because a package happened to match by accident.
	if o.useAAB {
		if needBundle || !exists("build/app.aab") {
			buildAAB()
			signAAB(o.keyStore, o.storePass, o.keyAlias, o.sigAlg)
		}
	} else {
		packed := filepath.Join("build", "bundle.zip")
		bundleSha := ""
		if exists(packed) {
			// an unreadable bundle is not a reason to keep an old apk: leaving the
			// digest empty forces align and sign again
			bundleSha, _ = fileSHA1(packed)
		}
		apkOk := exists(filepath.Join("build", "app.apk"))
		if !o.incremental || !apkOk || bundleSha != DataTable.BundleSha1 {
			alignAPK()
			signAPK(o.keyStore, o.storePass, o.keyAlias)
			if o.incremental {
				DataTable.BundleSha1 = bundleSha
			}
		}
	}

	if o.incremental {
		saveTable()
	}
}

// clean simply deletes the build dir
func clean() {
	LogI("clean", "removing build/*")
	os.RemoveAll("build")
}

// prepare verifies the project paths and setup the build dir
func prepare() {
	mustExist := func(path string) {
		if _, err := os.Stat(path); err != nil {
			LogF("build", err)
		}
	}

	mustExist("src")
	mustExist("res")
	mustExist("AndroidManifest.xml")

	mustMkdir := func(path string) {
		// only ignore if error is "already exist"
		if err := os.MkdirAll(path, 0755); err != nil && !os.IsExist(err) {
			LogF("build", err)
		}
	}

	mustMkdir(filepath.Join("build", "flats"))
	mustMkdir(filepath.Join("build", "classes"))
}

// compileRes compiles the xml files in res dir
func compileRes(incremental bool) bool {
	defer timeIt("aapt2 compile")()
	res, _ := getFiles("res", "", incremental)
	if len(res) < 1 {
		return false
	}

	LogI("build", "compiling resources")
	args := []string{"compile", "-o", filepath.Join("build", "flats")}
	args = append(args, res...)
	cmd := exec.Command(aapt2Path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}
	if incremental {
		updateRecords(res)
	}
	return true
}

// bundleRes bundles all the flat files into apk and generates the java files aapt
// writes into src/. It reports whether any of them actually changed, which is what
// makes the sources that read those constants stale. Only the incremental build
// cares: a plain one recompiles everything anyway and must not touch the state file.
func bundleRes(useAAB bool, incremental bool) bool {
	defer timeIt("aapt2 link")()
	flats, _ := getFiles("build/flats", ".flat", false)
	if len(flats) < 1 {
		return false
	}

	var before map[string]genRecord
	if incremental {
		before = snapshotGenJava()
	}

	LogI("build", "bundling resources")
	args := []string{"link", "-I", androidJar, "--manifest", "AndroidManifest.xml", "-o", "build", "--java", "src", "--output-to-dir"}
	if useAAB {
		args = append(args, "--proto-format")
	}
	args = append(args, flats...)
	cmd := exec.Command(aapt2Path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}

	if !incremental {
		return false
	}
	return settleGenJava(before)
}

// genRecord is what a file aapt generates looked like before the link rewrote it.
type genRecord struct {
	digest  string
	modTime time.Time
}

// genJavaFile names what aapt2 link writes into the java output dir. R.java holds
// the resource ids; Manifest.java is the old aapt artifact for the <permission>
// elements of the merged manifest, and aapt2 still writes it. Both are inputs to
// javac, so a rewrite with unchanged content must not look like an edit.
func genJavaFile(name string) bool {
	return name == "R.java" || name == "Manifest.java"
}

// snapshotGenJava records every file aapt generates under src/ before the link.
func snapshotGenJava() map[string]genRecord {
	records := map[string]genRecord{}
	filepath.Walk("src", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !genJavaFile(info.Name()) {
			return nil
		}
		digest, err := fileSHA1(path)
		if err != nil {
			return nil
		}
		records[path] = genRecord{digest: digest, modTime: info.ModTime()}
		return nil
	})
	return records
}

// settleGenJava undoes the damage of aapt2 rewriting files that did not change:
// link rewrites all of them on every run, and the fresh mtime would make every one
// of them look like an edit to the incremental build. Content that really changed
// is left alone and reported, because then the constants are different and every
// source using them must be recompiled.
func settleGenJava(before map[string]genRecord) bool {
	seen := map[string]bool{}
	changed := false

	filepath.Walk("src", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !genJavaFile(info.Name()) {
			return nil
		}
		seen[path] = true
		digest, err := fileSHA1(path)
		if err != nil {
			return nil
		}
		old, existed := before[path]
		switch {
		case !existed:
			LogD("Incremental", "aapt produced a new file", path)
			changed = true
		case old.digest != digest:
			LogD("Incremental", "aapt changed the content of", path)
			changed = true
		default:
			if err := os.Chtimes(path, old.modTime, old.modTime); err != nil {
				LogW("Incremental", "could not restore mtime", path, err.Error())
			}
		}
		return nil
	})

	for path := range before {
		if !seen[path] {
			LogD("Incremental", "aapt stopped producing", path)
			changed = true
		}
	}
	return changed
}

func compileKotlin(incremental bool) bool {
	defer timeIt("kotlinc")()
	kotlins, _ := getFiles("src", "kt", incremental)
	if len(kotlins) < 1 {
		return false
	}

	LogI("build", "compiling kotlin files")

	// the classpath always needs every jar, not only the ones that changed
	jars := strings.Join(allJars(), ":")

	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", androidJar + ":" + jars, "src"}
	args = append(args, kotlins...)
	cmd := exec.Command(kotlincPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), args)
	}
	if len(out) > 0 {
		LogW("kotlinc", string(out))
	}
	if incremental {
		updateRecords(kotlins)
	}
	return true
}

// allJars returns every jar of the project. It is used for classpaths, where
// an incremental subset would silently drop dependencies.
func allJars() []string {
	jars, _ := getFiles("jar", "jar", false)
	return jars
}

// compileJava compiles java files in src dir and uses jar files in the jar dir as classpath
func compileJava(incremental bool) bool {
	defer timeIt("javac")()
	javas, _ := getFiles("src", "java", incremental)
	if len(javas) < 1 {
		return false
	}
	LogD("Incremental", "javac recompiles", len(javas), "java files")
	LogI("build", "compiling java files")

	jars := strings.Join(allJars(), ":")

	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", androidJar + ":" + filepath.Join("build", "classes") + ":" + jars}
	args = append(args, javas...)
	cmd := exec.Command(javacPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), args)
	}
	// a compiler that exits 0 can still say something the build should not swallow:
	// deprecation notes and unchecked warnings are the only notice a source edit gets
	// before it ships
	if len(out) > 0 {
		LogW("javac", string(out))
	}
	if incremental {
		updateRecords(javas)
	}
	return true
}

// d8Args are the flags every d8 invocation needs, shared by the per jar cache
// and by the merge: desugaring differently in the two stages would produce a dex
// that no single pass would have produced.
func d8Args(api int) []string {
	args := []string{"--lib", androidJar, "--release"}
	if api > 0 {
		args = append(args, "--min-api", strconv.Itoa(api))
	}
	return args
}

// classpathArgs are the --classpath flags that let a d8 call resolve the types of the
// jars it is not dexing. A dependency that desugars a default or static interface method
// against another dependency, and every project class that uses a dependency, need those
// types present or d8 silently drops the class: measured on the demo, handing the
// constraintlayout jar to a bucket d8 without the rest of jar/ on the classpath lost
// androidx.constraintlayout.helper.widget.Carousel from the apk (and its warning, "Type
// ... was not found, it is required for ... desugaring", is the only trace). skip names
// the jars already given as inputs, so a bucket does not list its own members here.
func classpathArgs(skip map[string]bool, jars []string) []string {
	args := []string{}
	for _, jar := range jars {
		if skip[jar] {
			continue
		}
		args = append(args, "--classpath", jar)
	}
	return args
}

// bundleJava turns the compiled sources of the project into build/classes.dex. The
// dependencies are not inputs of this call above API 21: they arrive as their own
// dex file, which is the whole point of splitting them, because a merge re-read
// every dependency byte on every edit of every source. They are still the classpath,
// or a project class that desugars against a dependency would be dropped from the dex.
// classes is the list build/classes was walked into, handed in so the tree is scanned
// once per build instead of once here and once by the caller that decides whether this
// call is needed at all.
func bundleJava(api int, jars []string, depsSplit bool, classes []string) {
	defer timeIt("d8 app")()
	if len(classes) < 1 {
		return
	}

	inputs := classes
	args := d8Args(api)
	if depsSplit {
		args = append(args, classpathArgs(nil, jars)...)
	} else {
		// a jar has to be dexed seeing the others as input, or d8 loses the
		// interfaces it needs to desugar default and static methods
		inputs = append(inputs, jars...)
	}

	LogI("build", "bundling classes")
	args = append(args, "--output", "build")
	args = append(args, inputs...)
	cmd := exec.Command(d8Path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), d8Path, args)
	}
	reportD8Drops(out)
}

// reportD8Drops prints what d8 said while succeeding. A class it could not resolve is
// not an error for d8: it writes the dex without that class and exits 0, so the only
// trace of a dependency silently left out of the apk is a warning on stderr. Measured
// when the buckets first went without a classpath: androidx.constraintlayout.helper.
// widget.Carousel disappeared from the apk and the build was green.
func reportD8Drops(out []byte) {
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "was not found") || strings.Contains(line, "required for") {
			LogW("d8", line)
		}
	}
}

// jarDigests is the content of jar/ as the build sees it: one sha1 per jar, in the
// order they are walked. Deciding from these instead of from timestamps is what
// keeps a rewritten-but-identical dependency from costing a merge, and what keeps a
// dependency restored with an old timestamp from escaping one.
func jarDigests(jars []string) []string {
	digests := []string{}
	for _, jar := range jars {
		digest, err := fileSHA1(jar)
		if err != nil {
			LogF("build", err, jar)
		}
		digests = append(digests, digest)
	}
	return digests
}

// jarsFingerprint names the exact dependency bytes a merge consumed. The api level
// is part of it, because d8 desugars differently per level and the dexes produced
// for one level are not valid inputs for another.
func jarsFingerprint(api int, digests []string) string {
	return sha1Text(strconv.Itoa(api) + "\n" + strings.Join(digests, "\n"))
}

// auxRoster is what lib/ and assets/ contribute to the bundle, decided by their
// names and their bytes. The stage that copies them only runs when the bundle is
// rebuilt, so without this the incremental build would not notice a native library
// or an asset appearing, and would keep shipping one that was deleted. Comparing
// bytes and not timestamps is what keeps a tool re-copying the same .so from
// costing a whole bundle. The per file digests come along because the bundle wants
// them to decide what it can copy raw, and those two trees are already read here.
func auxRoster() (string, map[string]string) {
	lines := []string{}
	digests := map[string]string{}
	for _, dir := range []string{"lib", "assets"} {
		files, _ := getFiles(dir, "", false)
		for _, file := range files {
			digest, err := fileSHA1(file)
			if err != nil {
				// an unreadable file is still a name in the roster: leaving it
				// out would hide the change instead of forcing the bundle
				digest = "unreadable:" + file
			}
			digests[file] = digest
			lines = append(lines, file+"\n"+digest)
		}
	}
	return sha1Text(strings.Join(lines, "\n")), digests
}

// dexDeps produces the dex files jar/ contributes, in a few independent sets called
// buckets. One call over every jar is the cheapest way to dex the whole set once, but
// it is also what an add or a remove would have to pay again in full: sixty jars took
// 224,8s pinned to three cpus. Splitting the set by the name of each jar keeps the
// buckets stable while the rest of jar/ changes, so adding a dependency re-dexes one
// bucket and not the lot. The cache holds several files per bucket because d8 splits
// by itself above the 64K method limit. It reports whether it had to run d8 for any
// bucket, which is when the dependencies really changed on disk.
func dexDeps(api int, jars []string, digests []string) ([]string, bool) {
	// the whole dependency pass, so an idle build that answered every bucket from the
	// cache stays under the threshold of timeIt and never logs
	defer timeIt("d8 deps")()
	// two jars with the same bytes define the same classes, and handed both to d8
	// abort the build on "defined multiple times": the whole set is deduped by digest
	// first, exactly as the single merge did, so a copy with another name costs nothing
	seen := map[string]bool{}
	uniqueJars := []string{}
	uniqueDigests := []string{}
	for i, jar := range jars {
		if seen[digests[i]] {
			continue
		}
		seen[digests[i]] = true
		uniqueJars = append(uniqueJars, jar)
		uniqueDigests = append(uniqueDigests, digests[i])
	}

	buckets := depBuckets(len(uniqueJars))
	dexes := []string{}
	produced := false
	for bucket := 0; bucket < buckets; bucket++ {
		memberJars := []string{}
		memberDigests := []string{}
		for i, jar := range uniqueJars {
			if bucketOf(jar, buckets) == bucket {
				memberJars = append(memberJars, jar)
				memberDigests = append(memberDigests, uniqueDigests[i])
			}
		}
		wrote, ran := dexBucket(api, memberJars, memberDigests, uniqueJars)
		dexes = append(dexes, wrote...)
		produced = produced || ran
	}
	return dexes, produced
}

// depBuckets is how many dex sets jar/ is split into: one call per bucket costs a d8
// start up, so a small set stays one call and a large one pays for the parallel
// savings of an add. Ten jars per bucket keeps the six buckets of a sixty jar project
// at about a sixth of the whole set each.
func depBuckets(jars int) int {
	buckets := (jars + 9) / 10
	if buckets > depsBucketsMax {
		return depsBucketsMax
	}
	if buckets < 1 {
		return 1
	}
	return buckets
}

// bucketOf puts one jar in one bucket by its name only: adding a dependency must not
// move the jars that are already there, or every bucket would lose its cache and an
// add would cost the whole set again. The count of buckets is stable while jar/ stays
// between the same thresholds, which is the only reshuffle this scheme pays for. The
// running hash is unsigned on purpose: a jar whose long name overflows a signed int
// would fold to a negative bucket, and a negative index matches none of the 0..buckets
// calls, so that dependency would be left out of the apk entirely.
func bucketOf(jar string, buckets int) int {
	var sum uint64
	for _, c := range filepath.Base(jar) {
		sum = sum*131 + uint64(c)
	}
	return int(sum % uint64(buckets))
}

// dexBucket dexes one bucket of jar/ into a directory named by its content and returns
// the dex files there, whether they were just written or found in the cache. A bucket
// the cache already answers contributes its dexes and reports false: the difference
// between an add that re-dexes one bucket and one that re-dexes the whole set is
// exactly that bool. A bucket whose jars hold no class is remembered by a marker so
// d8 is not handed them again to produce nothing. allJars is every jar of the set, of
// which the ones that are not in this bucket become the classpath: a class desugaring
// against a type another bucket holds would otherwise be dropped by d8.
func dexBucket(api int, jars []string, digests []string, allJars []string) ([]string, bool) {
	if len(jars) < 1 {
		return nil, false
	}
	// the cache key is the api level and the bytes of this bucket only: re-dexing a
	// bucket because an unrelated bucket changed is exactly the whole-set cost the
	// split exists to avoid. A cross bucket desugaring bridge against a type that
	// later changes shape is not recomputed here; that is the price of the split and
	// is called out in the README.
	dir := filepath.Join("build", "dexcache", "deps-"+sha1Text(strconv.Itoa(api)+"\n"+strings.Join(digests, "\n")))
	// a bucket that the cache answers costs milliseconds and a bucket that has to go
	// through d8 costs up to a minute and a half, so the stage name carries the
	// directory: without it the log says only that the build was slow
	defer timeIt("d8 bucket " + filepath.Base(dir))()
	if exists(dir) {
		// touching the mtime is what keeps the buckets of the roster in use the
		// newest in the cache, so the prune never throws away one about to be read
		now := time.Now()
		if err := os.Chtimes(dir, now, now); err != nil {
			LogW("build", "could not refresh", dir, err.Error())
		}
		return dexFilesIn(dir), false
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		LogF("build", err, dir)
	}

	skip := map[string]bool{}
	for _, jar := range jars {
		skip[jar] = true
	}
	LogI("build", "dexing dependencies")
	args := d8Args(api)
	args = append(args, classpathArgs(skip, allJars)...)
	args = append(args, "--output", dir)
	args = append(args, jars...)
	cmd := exec.Command(d8Path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// d8 still aborts on a class two jars of the same bucket both define, which
		// is the merge's guarantee surviving inside a bucket; across buckets the
		// shadowedClass guard only covers the project, not a jar shadowing another
		LogF("build", string(out), d8Path, args)
	}
	reportD8Drops(out)
	dexes := dexFilesIn(dir)
	if len(dexes) == 0 {
		if err := os.WriteFile(dir+".nodex", nil, 0644); err != nil {
			LogF("build", err, dir)
		}
	}
	pruneDepsCache()
	return dexes, true
}

// dexFilesIn lists the dex files of a d8 output dir, ordered by their index: d8
// names them classes.dex, classes2.dex, ... and the bundle has to continue that
// numbering after the dex of the project.
func dexFilesIn(dir string) []string {
	dexes, err := filepath.Glob(filepath.Join(dir, "classes*.dex"))
	if err != nil {
		LogF("build", err, dir)
	}
	sort.Slice(dexes, func(i, j int) bool { return dexIndex(dexes[i]) < dexIndex(dexes[j]) })
	return dexes
}

// dexIndex is the number a dex entry of the apk carries: classes.dex is the first.
func dexIndex(name string) int {
	base := strings.TrimSuffix(filepath.Base(name), ".dex")
	if base == "classes" {
		return 1
	}
	index, err := strconv.Atoi(strings.TrimPrefix(base, "classes"))
	if err != nil || index < 2 {
		// not something d8 wrote: keep it at the end instead of silently
		// renumbering the files that come after it
		return 1 << 30
	}
	return index
}

// dexEntryName is the apk name of the index-th dex file, 1 being classes.dex.
func dexEntryName(index int) string {
	if index <= 1 {
		return "classes.dex"
	}
	return "classes" + strconv.Itoa(index) + ".dex"
}

// shadowedClass reports a class the project compiles that a dependency also ships,
// reading nothing but the central directories of the jars. The single merge used to
// catch this by aborting on "defined multiple times"; with two dex files the runtime
// picks whichever one it reads first and no build log ever mentions it.
func shadowedClass(jars []string) (string, string, bool) {
	root := filepath.Join("build", "classes")
	own, _ := getFiles(root, ".class", false)
	if len(own) < 1 || len(jars) < 1 {
		return "", "", false
	}
	names := map[string]bool{}
	for _, class := range own {
		name, err := filepath.Rel(root, class)
		if err != nil {
			continue
		}
		names[strings.TrimSuffix(name, ".class")] = true
	}
	for _, jar := range jars {
		held, err := jarClasses(jar)
		if err != nil {
			// an unreadable jar is d8's problem to report, not a reason to skip
			// the check over every other dependency
			LogW("build", "could not read", jar, err.Error())
			continue
		}
		for _, name := range held {
			if names[name] {
				return strings.ReplaceAll(name, "/", ".") + " in " + filepath.Base(jar), jar, true
			}
		}
	}
	return "", "", false
}

// jarClasses lists the classes a jar holds, as paths without the .class suffix,
// which is how the compiled tree of the project names them too.
func jarClasses(jar string) ([]string, error) {
	reader, err := zip.OpenReader(jar)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	names := []string{}
	for _, file := range reader.File {
		name := file.Name
		if !strings.HasSuffix(name, ".class") || strings.HasPrefix(name, "META-INF/") ||
			strings.HasSuffix(name, "/module-info.class") || strings.HasSuffix(name, "/package-info.class") {
			// d8 discards all three: a collision there is not a collision
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".class"))
	}
	return names, nil
}

// pruneDepsCache drops the oldest dexed dependency buckets. The cache now holds jar/
// split into buckets keyed by their content, so it grows with every bucket version ever
// tried, while the newest few are what an add and a remove back and forth needs to skip
// the work. dexBucket refreshes the mtime of every bucket it reads, so the buckets of
// the roster in use are never the oldest thing here.
func pruneDepsCache() {
	dir := filepath.Join("build", "dexcache")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sets := []fs.DirEntry{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "deps-") {
			// the per jar dexes of the layout this replaced: one file per
			// dependency, nobody reads them any more and they are megabytes
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				LogW("build", "could not prune", entry.Name(), err.Error())
			}
			continue
		}
		// only the sets themselves: a .nodex marker is one byte and counting it
		// would push a real set out of the cache
		if entry.IsDir() {
			sets = append(sets, entry)
		}
	}
	if len(sets) <= depsCacheKeep {
		return
	}
	sort.Slice(sets, func(i, j int) bool {
		left, err := sets[i].Info()
		if err != nil {
			return true
		}
		right, err := sets[j].Info()
		if err != nil {
			return false
		}
		return left.ModTime().Before(right.ModTime())
	})
	for _, entry := range sets[:len(sets)-depsCacheKeep] {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			LogW("build", "could not prune", path, err.Error())
		}
		_ = os.Remove(path + ".nodex")
	}
}

// bundleEntry is one file of the package: the path it comes from, the name it carries
// inside the apk, whether it is deflated or stored, and the content it was written
// from last time.
type bundleEntry struct {
	src      string
	name     string
	compress bool
	digest   string
}

// reusePlan answers, for each planned entry in order, whether the previous package can hand
// it over as it is. That happens only when the old package holds the name, the source still
// hashes to what the last bundle wrote from it, and an earlier entry has not already taken
// that name: a file whose bytes changed, an entry the old package lacks and a cold bundle all
// go through the deflater. The returned map is what the state records for the next build, so
// an entry that leaves the plan leaves the record with it, and a source that comes back with
// other bytes is written again instead of reused from a stale copy.
func reusePlan(plan []bundleEntry, old map[string]bool, recorded map[string]string) (reuse []bool, entries map[string]string) {
	reuse = make([]bool, len(plan))
	entries = make(map[string]string, len(plan))
	taken := map[string]bool{}
	for i, entry := range plan {
		entries[entry.name] = entry.digest
		if old[entry.name] && !taken[entry.name] && recorded[entry.name] == entry.digest {
			reuse[i] = true
			taken[entry.name] = true
		}
	}
	return reuse, entries
}

// buildBundle writes build/bundle.zip. The entries are planned first and then written
// in that fixed order, so the package is decided by content alone. An entry whose
// source still hashes to what the last bundle took from it is copied over as it is,
// with neither an inflate nor a deflate: that is what keeps a one line source edit from
// recompressing a hundred and thirty six megabytes of assets that did not change.
// The new package goes through a temporary file and a rename, because the previous one
// is still open as the source of those raw copies while this one is being written.
func buildBundle(useAAB bool, depDexes []string, auxDigests map[string]string) {
	defer timeIt("bundle zip")()

	// the digest of a file, taken from the roster lib/ and assets/ already hashed
	// when the bundle is being decided, so those trees are read once per build
	digestOf := func(path string) string {
		if digest, ok := auxDigests[path]; ok {
			return digest
		}
		digest, err := fileSHA1(path)
		if err != nil {
			// an unreadable file is keyed by its name: it cannot match what the
			// last bundle wrote, so it is copied again instead of reused
			return "unreadable:" + path
		}
		return digest
	}

	plan := []bundleEntry{}
	add := func(src, name string, compress bool) {
		plan = append(plan, bundleEntry{src: src, name: name, compress: compress, digest: digestOf(src)})
	}

	// the apk dex entries are numbered from one: the classes of the project take the
	// first and the dependencies continue after it. A project without sources has no
	// app dex, so its dependencies then start at the first one.
	if useAAB {
		add(filepath.Join("build", "AndroidManifest.xml"), filepath.Join("manifest", "AndroidManifest.xml"), true)
	} else {
		add(filepath.Join("build", "AndroidManifest.xml"), "AndroidManifest.xml", true)
	}
	appDex := filepath.Join("build", "classes.dex")
	dexDir := ""
	if useAAB {
		dexDir = "dex"
	}
	next := 1
	if exists(appDex) {
		add(appDex, filepath.Join(dexDir, dexEntryName(next)), true)
		next++
	}
	for _, dex := range depDexes {
		add(dex, filepath.Join(dexDir, dexEntryName(next)), true)
		next++
	}

	if useAAB {
		add(filepath.Join("build", "resources.pb"), "resources.pb", true)
	} else {
		add(filepath.Join("build", "resources.arsc"), "resources.arsc", false)
	}

	files, _ := getFiles(filepath.Join("build", "res"), "", false)
	for _, f := range files {
		r, err := filepath.Rel("build", f)
		if err != nil {
			LogF("build", err)
		}
		add(f, r, true)
	}

	files, _ = getFiles("assets", "", false)
	if len(files) > 0 {
		LogI("build", "bundling assets")
	}
	for _, f := range files {
		add(f, f, true)
	}

	files, _ = getFiles("lib", "", false)
	if len(files) > 0 {
		LogI("build", "bundling native libs")
	}
	for _, f := range files {
		add(f, f, true)
	}

	// opened before anything is written: the old package is the source of the raw
	// copies, and it must keep its bytes while the new one is being built
	packed := filepath.Join("build", "bundle.zip")
	// no previous package, or one this build cannot read, only matters through
	// prevErr: then every entry is written from its source, as a cold bundle does
	previous, prevErr := zip.OpenReader(packed)

	outFile, err := os.Create(packed + ".tmp")
	if err != nil {
		LogF("build", err)
	}

	w := zip.NewWriter(outFile)

	addFileToZip := func(s, d string, compress bool) {
		var dst io.Writer
		if compress {
			dst, err = w.Create(d)
		} else {
			dst, err = w.CreateHeader(&zip.FileHeader{
				Name:   d,
				Method: zip.Store,
			})
		}
		if err != nil {
			LogF("build", err)
		}

		src, err := os.Open(s)
		if err != nil {
			LogF("build", err)
		}

		_, err = io.Copy(dst, src)
		if err != nil {
			LogF("build", err)
		}
	}

	byName := map[string]*zip.File{}
	oldNames := map[string]bool{}
	if prevErr == nil {
		for _, f := range previous.File {
			byName[f.Name] = f
			oldNames[f.Name] = true
		}
	}
	reuse, entries := reusePlan(plan, oldNames, DataTable.BundleEntries)
	reused := 0
	for i, entry := range plan {
		if reuse[i] {
			if err := w.Copy(byName[entry.name]); err != nil {
				LogF("build", err, entry.name)
			}
			reused++
			continue
		}
		addFileToZip(entry.src, entry.name, entry.compress)
	}
	if reused > 0 {
		LogD("build", "copied", reused, "entries from the previous package")
	}

	if err := w.Close(); err != nil {
		LogF("build", err)
	}
	if err := outFile.Close(); err != nil {
		LogF("build", err)
	}
	if prevErr == nil {
		previous.Close()
	}
	if err := os.Rename(packed+".tmp", packed); err != nil {
		LogF("build", err)
	}
	DataTable.BundleEntries = entries
}
