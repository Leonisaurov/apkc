package main

import "testing"

func TestWatchIgnoresEveryFileAaptGenerates(t *testing.T) {
	for _, path := range []string{"src/com/myapp/R.java", "src/com/myapp/Manifest.java"} {
		if !isGenerated(path) {
			t.Errorf("%s is a product of aapt2 link: watching it makes the round that wrote it trigger the next", path)
		}
	}

	for _, path := range []string{"src/com/myapp/MainActivity.java", "src/com/myapp/App.kt", "src/com/myapp/Constants.java", "AndroidManifest.xml", "res/values/strings.xml"} {
		if isGenerated(path) {
			t.Errorf("%s is a source of the project and must trigger a rebuild", path)
		}
	}
}

func TestWatchIgnoresTheTreesApkcWrites(t *testing.T) {
	for _, path := range []string{"build/classes/com/myapp/App.class", "build/incremental.json", "src/com/myapp/gen/R.java", ".gradle/caches/x.jar"} {
		if !isGenerated(path) {
			t.Errorf("%s is tool state or output, it must not reach the watcher", path)
		}
	}
}
