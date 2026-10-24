package main

import (
	"archive/zip"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
	prepare()

	var settedType, resStale, classStale bool
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
		resStale, classStale = pruneDeleted()
		if resStale || classStale {
			saveTable()
		}
	}

	if settedType {
		LogI("Diagnostic", "Detected proyect type changed.", "Rebuilding.")
	}

	// the manifest holds the package name and the resource ids, so every
	// resource and source has to be recompiled when it changes
	if o.incremental && hasChanged("AndroidManifest.xml") {
		ns, _ := modTime("AndroidManifest.xml")
		DataTable.Timestamps["AndroidManifest.xml"] = ns
		_, unchanged := getFiles("res", "", true)
		markAsChanged(unchanged)
		_, unchanged = getFiles("src", "kt", true)
		markAsChanged(unchanged)
		_, unchanged = getFiles("src", "java", true)
		markAsChanged(unchanged)
		saveTable()
	}

	// an intermediate deleted by hand must be produced again, no matter what
	// the timestamps say about the inputs
	linkedFile := filepath.Join("build", "resources.arsc")
	if o.useAAB {
		linkedFile = filepath.Join("build", "resources.pb")
	}
	resMissing := !exists(linkedFile)
	dexMissing := !exists(filepath.Join("build", "classes.dex"))

	compRes := compileRes(o.incremental)
	if !o.incremental || firstRun || settedType || compRes || resStale || resMissing {
		bundleRes(o.useAAB)
	}

	compSource := compileKotlin(o.incremental)
	compSource = compileJava(o.incremental) || compSource
	// a dependency is part of the classpath and of the dex input, so it makes
	// the classes stale even when no source changed
	jarChanged := false
	if o.incremental {
		for _, jar := range allJars() {
			if hasChanged(jar) {
				jarChanged = true
				break
			}
		}
	}
	if !o.incremental || firstRun || settedType || compSource || classStale || jarChanged || dexMissing {
		bundleJava()
		if o.incremental {
			updateRecords(allJars())
		}
	}

	needBundle := !o.incremental || firstRun || settedType || compRes || compSource || resStale || classStale ||
		jarChanged || resMissing || dexMissing || !exists("build/bundle.zip")
	if needBundle {
		buildBundle(o.useAAB)
	}

	if o.useAAB {
		if needBundle || !exists("build/app.aab") {
			buildAAB()
			signAAB(o.keyStore, o.storePass, o.keyAlias, o.sigAlg)
		}
	} else {
		if needBundle || !exists("build/app.apk") {
			alignAPK()
			signAPK(o.keyStore, o.storePass, o.keyAlias)
		}
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

// bundleRes bundles all the flat files into apk and generates R.* id file for java
func bundleRes(useAAB bool) {
	flats, _ := getFiles("build/flats", ".flat", false)
	if len(flats) < 1 {
		return
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
}

func compileKotlin(incremental bool) bool {
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
	javas, _ := getFiles("src", "java", incremental)
	if len(javas) < 1 {
		return false
	}
	LogI("build", "compiling java files")

	jars := strings.Join(allJars(), ":")

	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", androidJar + ":" + filepath.Join("build", "classes") + ":" + jars}
	args = append(args, javas...)
	cmd := exec.Command(javacPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), args)
	}
	if incremental {
		updateRecords(javas)
	}
	return true
}

// bundleJava bundles compiled java class files and external jar files into apk
func bundleJava() {
	classes, _ := getFiles(filepath.Join("build", "classes"), ".class", false)
	if len(classes) < 1 {
		return
	}

	jars, _ := getFiles("jar", ".jar", false)

	LogI("build", "bundling classes and jars")
	args := []string{"--lib", androidJar, "--release", "--output", "build"}
	args = append(args, classes...)
	args = append(args, jars...)
	cmd := exec.Command(d8Path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), d8Path, args)
	}
}

func buildBundle(useAAB bool) {
	outFile, err := os.Create(filepath.Join("build", "bundle.zip"))
	if err != nil {
		LogF("build", err)
	}
	defer outFile.Close()

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

	if useAAB {
		addFileToZip(filepath.Join("build", "AndroidManifest.xml"), filepath.Join("manifest", "AndroidManifest.xml"), true)
		addFileToZip(filepath.Join("build", "classes.dex"), filepath.Join("dex", "classes.dex"), true)
		addFileToZip(filepath.Join("build", "resources.pb"), "resources.pb", true)
	} else {
		addFileToZip(filepath.Join("build", "AndroidManifest.xml"), "AndroidManifest.xml", true)
		addFileToZip(filepath.Join("build", "classes.dex"), "classes.dex", true)
		addFileToZip(filepath.Join("build", "resources.arsc"), "resources.arsc", false)
	}

	files, _ := getFiles(filepath.Join("build", "res"), "", false)
	for _, f := range files {
		r, err := filepath.Rel("build", f)
		if err != nil {
			LogF("build", err)
		}
		addFileToZip(f, r, true)
	}

	files, _ = getFiles("assets", "", false)
	if len(files) > 0 {
		LogI("build", "bundling assets")
	}
	for _, f := range files {
		addFileToZip(f, f, true)
	}

	files, _ = getFiles("lib", "", false)
	if len(files) > 0 {
		LogI("build", "bundling native libs")
	}
	for _, f := range files {
		addFileToZip(f, f, true)
	}

	err = w.Close()
	if err != nil {
		LogF("build", err)
	}
}
