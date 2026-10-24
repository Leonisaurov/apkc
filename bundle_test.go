package main

import (
	"path/filepath"
	"testing"
)

func entry(name, digest string) bundleEntry {
	return bundleEntry{src: filepath.Join("build", name), name: name, compress: true, digest: digest}
}

// key is what the record of an entry has to say for that entry to be handed over
// by the next bundle: the bytes it came from and the method it was written with.
func key(e bundleEntry) string { return e.contentKey() }

func TestReusePlanKeepsOnlyTheUnchangedEntries(t *testing.T) {
	plan := []bundleEntry{entry("classes.dex", "aa"), entry("assets/big.bin", "bb"), entry("resources.arsc", "cc")}
	old := map[string]bool{"classes.dex": true, "assets/big.bin": true, "resources.arsc": true}
	recorded := map[string]string{
		"classes.dex":    key(plan[0]),
		"assets/big.bin": "stale",
		"resources.arsc": key(plan[2]),
	}

	reuse, entries := reusePlan(plan, old, recorded)

	want := []bool{true, false, true}
	if len(reuse) != len(want) {
		t.Fatalf("reuse = %v, want one answer per entry", reuse)
	}
	for i := range want {
		if reuse[i] != want[i] {
			t.Errorf("reuse of %s = %v, want %v", plan[i].name, reuse[i], want[i])
		}
	}
	if len(entries) != len(plan) {
		t.Fatalf("the record must cover every planned entry, got %d", len(entries))
	}
	if entries["assets/big.bin"] != key(plan[1]) {
		t.Errorf("the record must hold the bytes this build wrote, got %q", entries["assets/big.bin"])
	}
}

// TestReusePlanDoesNotCarryAnEntryAcrossAPackingFlip is the android:
// extractNativeLibs case: the file is untouched and only the method changes, and
// a deflated .so left inside a package that promises the runtime can map it is
// an apk that installs and then fails to load the library.
func TestReusePlanDoesNotCarryAnEntryAcrossAPackingFlip(t *testing.T) {
	deflated := entry("lib/arm64-v8a/libfoo.so", "aa")
	stored := bundleEntry{src: filepath.Join("build", "lib/arm64-v8a/libfoo.so"), name: "lib/arm64-v8a/libfoo.so", compress: false, digest: "aa"}
	plan := []bundleEntry{stored}
	old := map[string]bool{stored.name: true}
	recorded := map[string]string{stored.name: key(deflated)}

	reuse, entries := reusePlan(plan, old, recorded)

	if reuse[0] {
		t.Error("an entry that changes method must be written again, not copied raw")
	}
	if entries[stored.name] != key(stored) {
		t.Errorf("the record must say which method this build used, got %q", entries[stored.name])
	}
}

func TestReusePlanOfAColdBundleWritesEverything(t *testing.T) {
	plan := []bundleEntry{entry("classes.dex", "aa")}

	reuse, _ := reusePlan(plan, map[string]bool{}, map[string]string{"classes.dex": "aa"})

	if reuse[0] {
		t.Fatal("an unreadable or missing previous package has nothing to copy raw")
	}
}

func TestReusePlanForgetsTheEntriesThatLeftThePlan(t *testing.T) {
	plan := []bundleEntry{entry("classes.dex", "aa")}
	old := map[string]bool{"classes.dex": true}
	recorded := map[string]string{"classes.dex": "aa", "assets/gone.bin": "zz"}

	_, entries := reusePlan(plan, old, recorded)

	if _, ok := entries["assets/gone.bin"]; ok {
		t.Error("an entry no longer bundled must not stay in the record")
	}
}

func TestReusePlanTakesANameOnlyOnce(t *testing.T) {
	plan := []bundleEntry{entry("assets/same.bin", "aa"), entry("assets/same.bin", "aa")}
	old := map[string]bool{"assets/same.bin": true}
	recorded := map[string]string{"assets/same.bin": key(plan[0])}

	reuse, _ := reusePlan(plan, old, recorded)

	if !reuse[0] {
		t.Error("the first occurrence of a known entry is the one to copy")
	}
	if reuse[1] {
		t.Error("the same copy cannot answer two entries of one name")
	}
}
