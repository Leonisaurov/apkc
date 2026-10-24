package main

import (
	"archive/zip"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// build compiles all the source code and bundles into apk file with dependencies
func build() {
	buildCmd := flag.NewFlagSet("build", flag.ExitOnError)

	useAAB := buildCmd.Bool("aab", false, "build aab instead of apk")
	keyStore := buildCmd.String("keystore", keyStorePath, "path to keystore")
	storePass := buildCmd.String("storepass", "android", "keystore password")
	keyAlias := buildCmd.String("keyalias", "androiddebugkey", "key alias to use")
	sigAlg := buildCmd.String("sigalg", "SHA256withRSA", "signature to use")

	buildCmd.Parse(os.Args[2:])

	prepare()
	compileRes()
	bundleRes(*useAAB)
	compileKotlin()
	compileJava()
	bundleJava()
	buildBundle(*useAAB)
	if *useAAB {
		buildAAB()
		signAAB(keyStore, storePass, keyAlias, sigAlg)
	} else {
		alignAPK()
		signAPK(keyStore, storePass, keyAlias)
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
func compileRes() {
	res := getFiles("res", "")
	LogI("build", "compiling resources")
	args := []string{"compile", "-o", filepath.Join("build", "flats")}
	args = append(args, res...)
	cmd := exec.Command(aapt2Path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out))
	}
}

// bundleRes bundles all the flat files into apk and generates R.* id file for java
func bundleRes(useAAB bool) {
	LogI("build", "bundling resources")

	// aapt2 --output-to-dir does not create res subdirectories on Windows
	entries, _ := os.ReadDir("res")
	for _, e := range entries {
		if e.IsDir() {
			os.MkdirAll(filepath.Join("build", "res", e.Name()), 0755)
		}
	}

	flats := getFiles("build/flats", ".flat")
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

func compileKotlin() {
	kotlins := getFiles("src", "kt")
	if len(kotlins) < 1 {
		return
	}

	LogI("build", "compiling kotlin files")

	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", joinPaths(append([]string{androidJar}, getFiles("jar", "jar")...)...), "src"}
	args = append(args, kotlins...)
	cmd := exec.Command(kotlincPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), args)
	}
}

// compileJava compiles java files in src dir and uses jar files in the jar dir as classpath
func compileJava() {
	LogI("build", "compiling java files")

	javas := getFiles("src", "java")

	classpath := append([]string{androidJar, filepath.Join("build", "classes")}, getFiles("jar", "jar")...)
	args := []string{"-d", filepath.Join("build", "classes"), "-classpath", joinPaths(classpath...)}
	args = append(args, javas...)
	cmd := exec.Command(javacPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		LogF("build", string(out), args)
	}
}

// bundleJava bundles compiled java class files and external jar files into apk
func bundleJava() {
	LogI("build", "bundling classes and jars")

	classes := getFiles(filepath.Join("build", "classes"), ".class")
	jars := getFiles("jar", ".jar")

	args := []string{"--lib", androidJar, "--release", "--output", "build"}
	args = append(args, classes...)
	args = append(args, jars...)
	// on Windows d8 is a .bat wrapper that cannot be exec'd directly
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		javaArgs := append([]string{"-cp", filepath.Join(toolsPath, "lib", "d8.jar"), "com.android.tools.r8.D8"}, args...)
		cmd = exec.Command(javaExe(), javaArgs...)
	} else {
		cmd = exec.Command(d8Path, args...)
	}
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

	files := getFiles(filepath.Join("build", "res"), "")
	for _, f := range files {
		r, err := filepath.Rel("build", f)
		if err != nil {
			LogF("build", err)
		}
		// zip entries must use forward slashes even on Windows
		addFileToZip(f, filepath.ToSlash(r), true)
	}

	files = getFiles("assets", "")
	if len(files) > 0 {
		LogI("build", "bundling assets")
	}
	for _, f := range files {
		addFileToZip(f, filepath.ToSlash(f), true)
	}

	files = getFiles("lib", "")
	if len(files) > 0 {
		LogI("build", "bundling native libs")
	}
	for _, f := range files {
		addFileToZip(f, filepath.ToSlash(f), true)
	}

	err = w.Close()
	if err != nil {
		LogF("build", err)
	}
}
