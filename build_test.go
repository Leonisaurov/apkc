package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingProjectPiecesNamesEveryAbsentOne(t *testing.T) {
	setupProject(t)

	// setupProject makes a build/ dir in an empty tree: all three are missing, and the
	// whole list is what the person who ran the command one directory too high needs
	if got := missingProjectPieces(); len(got) != 3 {
		t.Fatalf("an empty directory reports %v, want the three pieces", got)
	}

	if err := os.MkdirAll(filepath.Join(".", "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".", "res"), 0755); err != nil {
		t.Fatal(err)
	}
	got := missingProjectPieces()
	if len(got) != 1 || got[0] != "AndroidManifest.xml" {
		t.Fatalf("with src and res present it reports %v, want only the manifest", got)
	}

	writeManifest(t, `<manifest package="com.myapp"><application/></manifest>`)
	if got := missingProjectPieces(); len(got) != 0 {
		t.Fatalf("a complete project reports %v, want nothing", got)
	}
}

func TestAndListPutsTheLastPieceBehindAnd(t *testing.T) {
	// the list goes into a sentence, so the grammar matters: three paths quoted with
	// commas alone read like a shell invocation rather than like what is missing
	cases := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"src"}, "`src`"},
		{[]string{"src", "res"}, "`src` and `res`"},
		{[]string{"AndroidManifest.xml", "src", "res"}, "`AndroidManifest.xml`, `src` and `res`"},
	}
	for _, c := range cases {
		if got := andList(c.names); got != c.want {
			t.Errorf("andList(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

func TestProjectPiecesAskForTheManifestFirst(t *testing.T) {
	// the manifest is what makes a directory an apkc project, so it has to be the first
	// name in the sentence a wrong-directory build prints
	if projectPieces[0] != "AndroidManifest.xml" {
		t.Fatalf("projectPieces starts with %q, want the manifest", projectPieces[0])
	}
}
