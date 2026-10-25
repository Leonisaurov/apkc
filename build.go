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
	coreLibrary bool
	keyStore    string
	storePass   string
	keyAlias    string
	sigAlg      string
	// signer picks who turns the plan into an apk: the JVM tools apkc has always
	// shelled out to, or the packager inside this binary
	signer string
}

// buildFlags are the command line switches that reach the stages. The three
// entry points used to declare them one by one, which is how a default or a
// description drifts apart between them, and how an option that belongs to the
// build ends up missing from one of the others.
type buildFlags struct {
	useAAB      *bool
	incremental *bool
	coreLibrary *bool
	keyStore    *string
	storePass   *string
	keyAlias    *string
	sigAlg      *string
	signer      *string
}

func registerBuildFlags(set *flag.FlagSet, withAAB bool) buildFlags {
	f := buildFlags{}
	if withAAB {
		// only the callers that can produce a bundle offer the switch: run, and
		// watch --run, install an apk, and a bundle is not installable as is
		f.useAAB = set.Bool("aab", false, "build an app bundle instead of an apk")
	}
	f.incremental = set.Bool("inc", false, "use the incremental build system")
	f.coreLibrary = set.Bool("core-library", false, "let d8 dex java.* classes (core library desugaring)")
	f.keyStore = set.String("keystore", keyStorePath, "path to the keystore")
	f.storePass = set.String("storepass", "android", "keystore password")
	f.keyAlias = set.String("keyalias", "androiddebugkey", "key alias to use")
	f.sigAlg = set.String("sigalg", "SHA256withRSA", "signature algorithm to use")
	f.signer = set.String("signer", signerJVM, "packaging and signing backend: jvm (zipalign and apksigner), go (v1 and v2 written inside apkc, needs a PKCS#12 keystore), or auto (go whenever it can, jvm otherwise)")
	return f
}

func (f buildFlags) opts() buildOpts {
	o := buildOpts{
		incremental: *f.incremental,
		coreLibrary: *f.coreLibrary,
		keyStore:    *f.keyStore,
		storePass:   *f.storePass,
		keyAlias:    *f.keyAlias,
		sigAlg:      *f.sigAlg,
		signer:      *f.signer,
	}
	if f.useAAB != nil {
		o.useAAB = *f.useAAB
	}
	return o
}

func build() {
	buildCmd := flag.NewFlagSet("build", flag.ExitOnError)
	flags := registerBuildFlags(buildCmd, true)

	buildCmd.Parse(os.Args[2:])
	_build(flags.opts())
}

// build compiles all the source code and bundles into apk file with dependencies
func _build(o buildOpts) {
	defer timeIt("total")()
	prepare()

	// d8 decides how much to desugar from this number, and without it it assumes
	// API 1 and rewrites every lambda of every dependency
	api := manifestMinAPI()

	// the manifest also dictates how lib/ travels inside the package, and that is
	// a fact about the bytes of the apk, not about what is stale: a plain build
	// needs it just as much as an incremental one
	storedLibs, libsDeclared := manifestNativeLibsStored()
	libs := nativeLibs{stored: storedLibs, declared: libsDeclared}

	// who writes the apk is decided by the api level and the keystore in front of it,
	// before any stage asks: --signer=auto means exactly that
	o.signer = resolveSigner(o.signer, o.keyStore, o.useAAB)

	var typeChanged, resStale, classStale, manifestStale, apiChanged bool
	firstRun := false
	if o.incremental {
		// without any record the whole build dir must be treated as missing,
		// even when every input looks unchanged
		firstRun = !exists(DataFile)
		if o.useAAB {
			typeChanged = setCompilationType("aab")
		} else {
			typeChanged = setCompilationType("apk")
		}
		resStale, classStale = pruneDeleted(api < 21)
		// the api level is an input of the project dex: --min-api decides how much
		// of the sources d8 desugars, so a minSdkVersion edit has to dex them again
		// even when no source changed (DataTable is loaded by the calls above)
		apiChanged = api != DataTable.AppApi
	}

	if typeChanged {
		LogI("Diagnostic", "The project type changed.", "Rebuilding.")
	}

	// The manifest is an input of the link and of nothing else. `aapt2 compile`
	// turns every resource file into a flat before the package name or a resource
	// id exists, so a new versionName or a declared permission does not
	// invalidate a single flat. The link is still forced, and what it rewrites
	// besides the merged manifest it embeds is R.java and Manifest.java under src/,
	// compared by content like any other tracked file: the sources pay javac only
	// when a constant really changed. Marking every flat and source changed here
	// used to cost a whole compile and javac pass over a `versionName` bump.
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
	if !o.incremental || firstRun || typeChanged || compRes || resStale || resMissing || manifestStale {
		// the files aapt generates are inputs like any other: they are compared by
		// content, and when a constant inside one of them really moved it is the
		// dependency index below that sends the sources back, not this call. Marking
		// every source here used to cost a whole javac pass over a resource that only
		// added an id, and a copy of a value that did not move was never stale.
		bundleRes(o.useAAB, o.incremental)
	}

	compiledKotlin, compKotlin := compileKotlin(o.incremental)
	compiledJava, compJava := compileJava(o.incremental)
	compSource := compKotlin || compJava
	// javac has no way to notice that what a source calls changed shape: its own
	// file did not. The compiled classes are the only record of that, and they
	// exist after this stage, so the callers get their own pass.
	if closeApiGap(o.incremental, append(append([]string{}, compiledJava...), compiledKotlin...)) {
		compSource = true
	}
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
		depDexes, depStale = dexDeps(api, o.coreLibrary, jars, digests)
	}
	// two different decisions that used to be one flag: whether the bundle has to take
	// the dexes of a changed dependency set, and whether the classes of the project
	// have to go through d8 again. Above API 21 the dependencies are not inputs of the
	// project dex, so an add that every bucket cache already answered was costing a
	// whole pass over the sources of the project for nothing
	depChanged := depStale || fingerprint != DataTable.DexInputs
	// below API 21 the jars are inputs of the one merge, so there a dependency does
	// change the dex the project shares with them, and it has to be dexed again
	appStale := !o.incremental || firstRun || typeChanged || compSource || classStale ||
		apiChanged || (depChanged && !splitDeps) || dexMissing
	if appStale {
		bundleJava(api, o.coreLibrary, jars, splitDeps, classes)
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
		roster, auxDigests = auxRoster(libs.stored)
		auxStale = roster != DataTable.AuxInputs
	}

	needBundle := !o.incremental || firstRun || typeChanged || compRes || compSource || resStale || classStale ||
		manifestStale || depChanged || auxStale || resMissing || dexMissing ||
		// the jvm backend reads its package back out of bundle.zip to align and sign
		// it, so a missing one is work. The go backend writes the apk from the plan and
		// has no intermediate to miss: asking for it there repackaged every idle round
		(o.signer != signerGo && !exists(filepath.Join("build", "bundle.zip")))
	if needBundle {
		if o.signer == signerGo && !o.useAAB {
			// the package is written below, from the same plan: a bundle.zip that
			// only exists to be read back and moved by zipalign would be wasted work
		} else {
			buildBundle(o.useAAB, depDexes, auxDigests, libs)
		}
		if o.incremental {
			DataTable.AuxInputs = roster
			// the bundle is what consumes a dependency set: recording the
			// fingerprint with the dex call would claim those bytes reached the apk
			// when the bundle was skipped
			DataTable.DexInputs = fingerprint
		}
	} else if o.incremental {
		LogD("build", "nothing to put in the package this round")
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
	} else if o.signer == signerGo {
		apkOk := exists(filepath.Join("build", "app.apk"))
		if needBundle || !apkOk {
			defer timeIt("package and sign go")()
			LogI("build", "packaging and signing inside apkc")
			plan := packagePlan(o.useAAB, depDexes, auxDigests, libs)
			reused, err := packageWithGo(plan, DataTable.BundleEntries, o.keyStore, o.storePass, o.keyAlias)
			if err != nil {
				LogF("build", err)
			}
			if reused > 0 {
				LogD("build", "copied", reused, "entries from the previous package")
			}
		} else if o.incremental {
			LogD("build", "the package did not change: the apk on disk stands")
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
		if mustPackageAgain(o.incremental, apkOk, bundleSha, DataTable.BundleSha1) {
			alignAPK(libs.stored)
			signAPK(o.keyStore, o.storePass, o.keyAlias)
			if o.incremental {
				DataTable.BundleSha1 = bundleSha
			}
		} else if o.incremental {
			// a touch of a resource recompiles and relinks it with the same bytes,
			// and the round would otherwise end in silence: the apk on disk is the
			// one this package was made from
			LogD("build", "the package bytes did not change: the apk on disk stands")
		}
	}

	if o.incremental {
		saveTable()
	}
}

// mustPackageAgain answers whether zipalign and apksigner have to run. Under
// --inc only the bytes of the package decide. Without --inc the answer is always
// yes, and that half is a contract rather than waste: a caller of the plain
// build has no other way to know the build ran, because a wrapper around it
// exits 0 even when apkc failed, so the apk getting newer IS the signal.
func mustPackageAgain(incremental, apkOk bool, bundleSha, recordedSha string) bool {
	return !incremental || !apkOk || bundleSha != recordedSha
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

	var before map[string]string
	if incremental {
		before = snapshotGenJava()
	}

	LogI("build", "bundling resources")
	// aapt2 link --output-to-dir does not clean the directory it writes: a resource
	// file that disappeared from res/ keeps its compiled copy under build/res, and
	// the bundle plans every file it finds there, so a deleted (or renamed) drawable
	// used to stay inside the apk with no entry in the resource table and no constant
	// in R.java. The directory is nothing but the output of this call, so it is
	// emptied right before it: an unchanged resource is written again with the same
	// bytes, so the bundle still reuses its entry.
	os.RemoveAll(filepath.Join("build", "res"))
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
	return genJavaChanged(before)
}

// genJavaFile names what aapt2 link writes into the java output dir. R.java holds
// the resource ids; Manifest.java is the old aapt artifact for the <permission>
// elements of the merged manifest, and aapt2 still writes it. Both are inputs to
// javac, so a rewrite with unchanged content must not look like an edit.
func genJavaFile(name string) bool {
	return name == "R.java" || name == "Manifest.java"
}

// snapshotGenJava records the content of every file aapt generates under src/,
// before the link rewrites them.
func snapshotGenJava() map[string]string {
	records := map[string]string{}
	filepath.Walk("src", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !genJavaFile(info.Name()) {
			return nil
		}
		digest, err := fileSHA1(path)
		if err != nil {
			return nil
		}
		records[path] = digest
		return nil
	})
	return records
}

// genJavaChanged reports whether the link wrote different constants. Content is
// the only thing that can answer it: aapt2 rewrites every one of these files on
// every run, and the ones whose bytes are identical must not look like an edit,
// while a real change invalidates every source, because javac inlines the R.*
// constants into the class files and a stale class would keep shipping an old id.
func genJavaChanged(before map[string]string) bool {
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
		case old != digest:
			LogD("Incremental", "aapt changed the content of", path)
			changed = true
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

// kotlinShortcomings names what a project with .kt files still lacks. Neither
// piece comes with the Android SDK: the compiler is its own install, and the
// stdlib is a jar the project has to carry in jar/, because android.jar does not
// contain kotlin.*. Without them the first thing the user sees is a wall of
// unresolved references, or an empty failure from a compiler that is not there.
func kotlinShortcomings(compiler string, jars []string) []string {
	missing := []string{}
	if _, err := exec.LookPath(compiler); err != nil {
		missing = append(missing, "kotlinc is not on PATH (looked for as "+compiler+")")
	}

	stdlib := false
	for _, jar := range jars {
		if strings.Contains(strings.ToLower(filepath.Base(jar)), "kotlin-stdlib") {
			stdlib = true
			break
		}
	}
	if !stdlib {
		missing = append(missing, "no kotlin-stdlib*.jar in jar/")
	}
	return missing
}

// compileKotlin answers the set the module went in with, which is wider than the
// files that changed: kotlinc has no per file mode, so every kotlin source of the
// project is recompiled whenever any of them moved.
func compileKotlin(incremental bool) ([]string, bool) {
	defer timeIt("kotlinc")()
	kotlins, _ := getFiles("src", ".kt", incremental)
	if len(kotlins) < 1 {
		return nil, false
	}

	// kotlinc has no per-file incremental mode at the command line: whenever the
	// stage runs, the whole module goes in. Listing the files instead of handing
	// over the directory with the changed ones on top keeps what is compiled and
	// what is recorded the same set, and the .java sources stay in the request
	// because kotlin resolves against them without compiling them.
	compiling := kotlins
	if incremental {
		compiling, _ = getFiles("src", ".kt", false)
	}
	javas, _ := getFiles("src", ".java", false)
	if incremental {
		LogD("Incremental", "kotlinc takes the whole module:", len(compiling), "kotlin files,",
			len(javas), "java sources for resolution")
	}
	sources := append(append([]string{}, compiling...), javas...)

	// the classpath always needs every jar, not only the ones that changed
	jars := allJars()
	for _, lack := range kotlinShortcomings(kotlincPath, jars) {
		LogW("kotlinc", lack, "needed to compile", len(compiling), "kotlin files")
	}

	LogI("build", "compiling kotlin files")

	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", classpath(append([]string{androidJar}, jars...))}
	args = append(args, sources...)
	cmd := exec.Command(kotlincPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), args)
	}
	if len(out) > 0 {
		LogW("kotlinc", string(out))
	}
	if incremental {
		updateRecords(compiling)
	}
	return compiling, true
}

// allJars returns every jar of the project. It is used for classpaths, where
// an incremental subset would silently drop dependencies.
func allJars() []string {
	jars, _ := getFiles("jar", ".jar", false)
	return jars
}

// classpath joins the entries the way the tool being called reads them, which is
// not always a colon: javac and kotlinc take the platform path list separator,
// and a hardcoded ":" leaves every dependency unreachable on Windows.
func classpath(entries []string) string {
	return strings.Join(entries, string(filepath.ListSeparator))
}

// compileJava compiles java files in src dir and uses jar files in the jar dir as
// classpath. It answers both what it handed to the compiler and whether it ran at
// all, because the dependency index needs to know which sources this round has
// already seen: a resource id change sends every source through javac, and the index
// would send the same files a second time for nothing.
func compileJava(incremental bool) ([]string, bool) {
	defer timeIt("javac")()
	javas, _ := getFiles("src", ".java", incremental)
	if len(javas) < 1 {
		return nil, false
	}
	LogD("Incremental", "javac recompiles", len(javas), "java files")
	LogI("build", "compiling java files")

	jars := allJars()

	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", classpath(append([]string{androidJar, filepath.Join("build", "classes")}, jars...))}
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
	return javas, true
}

// closeApiGap gives javac the input it cannot see: the shape of the classes its
// sources link against. It runs after the sources whose bytes moved have compiled,
// reads what those classes now present, and sends every caller of a shape that
// moved through the compiler again.
//
// A shape that did not move costs nothing, and the first build after a wiped state
// only records the index: every source went through the compiler that round anyway,
// so there is no caller left to chase. compiled is what this round already handed
// over — a new resource id sends every source through javac, and answering that with
// a second pass over the same files would be the index costing a whole compile.
func closeApiGap(incremental bool, compiled []string) bool {
	if !incremental {
		return false
	}

	classes, unread := classIndex(filepath.Join("build", "classes"))
	if len(unread) > 0 {
		LogD("Incremental", "the shape of", len(unread), "class files could not be read, and they name nothing")
	}
	recorded := DataTable.ClassAPI
	recordedFields := DataTable.ClassFields
	DataTable.ClassAPI = apiDigests(classes)
	DataTable.ClassFields = fieldValues(classes)
	if len(recorded) == 0 {
		return false
	}

	// a constant whose value moved, or that stopped existing, is a change every copy
	// of it has to see — and the copies cannot be found, because javac writes the
	// number into the reader and keeps no reference to the field it came from. So the
	// answer is the whole source tree, and nothing else: an added id that moved no
	// value is invisible to the readers and costs nothing here.
	moved := constantsMoved(classes, recordedFields)

	callers := withoutSources(staleCallers(classes, recorded), compiled)
	if len(callers) == 0 && !moved {
		return false
	}

	if moved {
		LogI("build", "recompiling every source: a constant value moved")
		_, unchanged := getFiles("src", ".java", true)
		_, unchangedKotlin := getFiles("src", ".kt", true)
		markAsChanged(append(append([]string{}, unchanged...), unchangedKotlin...))
	} else {
		LogI("build", "recompiling the callers of a class that changed shape")
		LogD("Incremental", len(callers), "sources call a class whose shape moved")
		markAsChanged(callers)
	}

	// each compiler takes exactly the callers of its own language: the records of
	// every source listed above were dropped, and the trees they walk only report
	// what has no record
	ran := false
	if _, ranKotlin := compileKotlin(true); ranKotlin {
		ran = true
	}
	if _, ranJava := compileJava(true); ranJava {
		ran = true
	}
	return ran
}

// withoutSources drops the paths a round has already compiled.
func withoutSources(paths []string, compiled []string) []string {
	done := make(map[string]bool, len(compiled))
	for _, path := range compiled {
		done[path] = true
	}
	out := []string{}
	for _, path := range paths {
		if !done[path] {
			out = append(out, path)
		}
	}
	return out
}

// d8Args are the flags every d8 invocation needs, shared by the per jar cache
// and by the merge: desugaring differently in the two stages would produce a dex
// that no single pass would have produced. coreLibrary is off by default because
// it lets a dependency carry java.* classes: right for the desugared core
// libraries, wrong for anything else, since d8 then stops telling you a package
// collides with the platform.
func d8Args(api int, coreLibrary bool) []string {
	args := []string{"--lib", androidJar, "--release"}
	if api > 0 {
		args = append(args, "--min-api", strconv.Itoa(api))
	}
	if coreLibrary {
		args = append(args, "--core-library")
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
func bundleJava(api int, coreLibrary bool, jars []string, depsSplit bool, classes []string) {
	defer timeIt("d8 app")()
	if len(classes) < 1 {
		return
	}

	inputs := classes
	args := d8Args(api, coreLibrary)
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
//
// The method lib/ is packed with is part of the roster too: flipping
// android:extractNativeLibs changes the package with not one byte of any .so
// moved, and a roster that stayed the same would let the bundle, and with it the
// apk, go unwritten.
func auxRoster(libStored bool) (string, map[string]string) {
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
			line := file + "\n" + digest
			if dir == "lib" {
				line += "\n" + zipMethod(libStored)
			}
			lines = append(lines, line)
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
func dexDeps(api int, coreLibrary bool, jars []string, digests []string) ([]string, bool) {
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
		wrote, ran := dexBucket(api, coreLibrary, memberJars, memberDigests, uniqueJars)
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
func dexBucket(api int, coreLibrary bool, jars []string, digests []string, allJars []string) ([]string, bool) {
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
	args := d8Args(api, coreLibrary)
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

// zipMethod names how an entry is carried inside the package. The record of the
// last bundle has to know it: an entry copied over from that package comes with
// the method it was written with, so a change of method must not reuse it.
func zipMethod(compress bool) string {
	if compress {
		return "deflate"
	}
	return "store"
}

// contentKey is what the record of a bundled entry holds: the bytes it was
// written from and the method it was written with. Both, because a package that
// flips android:extractNativeLibs has to write that entry again even though the
// file did not change: copying the old bytes would keep the previous method, and
// a deflated .so behind extractNativeLibs=false is a package the runtime cannot
// map, which is worse than a rebuild.
func (e bundleEntry) contentKey() string {
	return e.digest + "|" + zipMethod(e.compress)
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
		key := entry.contentKey()
		entries[entry.name] = key
		if old[entry.name] && !taken[entry.name] && recorded[entry.name] == key {
			reuse[i] = true
			taken[entry.name] = true
		}
	}
	return reuse, entries
}

// nativeLibs is what the manifest dictates about the .so of lib/ inside the
// package: stored and page aligned when it promises the runtime can map them
// from the apk, deflated when it expects the package manager to unpack them.
type nativeLibs struct {
	stored   bool
	declared bool
}

// buildBundle writes build/bundle.zip. The entries are planned first and then written
// in that fixed order, so the package is decided by content alone. An entry whose
// source still hashes to what the last bundle took from it is copied over as it is,
// with neither an inflate nor a deflate: that is what keeps a one line source edit from
// recompressing a hundred and thirty six megabytes of assets that did not change.
// The new package goes through a temporary file and a rename, because the previous one
// is still open as the source of those raw copies while this one is being written.
// packagePlan is the list of files the package is made of, in the order they go in.
// Both packagers read it: the JVM one writes it into bundle.zip for zipalign and
// apksigner to rewrite afterwards, the Go one writes it straight into the apk with
// the alignment each entry needs and signs it there. One list, so the two cannot
// disagree about what is in the package or in which order.
func packagePlan(useAAB bool, depDexes []string, auxDigests map[string]string, libs nativeLibs) []bundleEntry {
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
		if !libs.declared {
			// the pairing is what breaks installs, and it is decided by an
			// attribute most project authors never wrote down
			LogD("build", "no android:extractNativeLibs in the manifest:", len(files),
				"libs go in deflated, so the package manager unpacks them at install")
		} else if libs.stored {
			LogD("build", "extractNativeLibs=false:", len(files), "libs go in stored, and page aligned")
		}
	}
	for _, f := range files {
		add(f, f, !libs.stored)
	}
	return plan
}

func buildBundle(useAAB bool, depDexes []string, auxDigests map[string]string, libs nativeLibs) {
	defer timeIt("bundle zip")()

	plan := packagePlan(useAAB, depDexes, auxDigests, libs)

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
