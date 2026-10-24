package main

import (
	"path/filepath"
	"testing"
)

func entry(name, digest string) bundleEntry {
	return bundleEntry{src: filepath.Join("build", name), name: name, compress: true, digest: digest}
}

func TestReusePlanKeepsOnlyTheUnchangedEntries(t *testing.T) {
	plan := []bundleEntry{entry("classes.dex", "aa"), entry("assets/big.bin", "bb"), entry("resources.arsc", "cc")}
	old := map[string]bool{"classes.dex": true, "assets/big.bin": true, "resources.arsc": true}
	recorded := map[string]string{"classes.dex": "aa", "assets/big.bin": "stale", "resources.arsc": "cc"}

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
	if entries["assets/big.bin"] != "bb" {
		t.Errorf("the record must hold the bytes this build wrote, got %q", entries["assets/big.bin"])
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
	recorded := map[string]string{"assets/same.bin": "aa"}

	reuse, _ := reusePlan(plan, old, recorded)

	if !reuse[0] {
		t.Error("the first occurrence of a known entry is the one to copy")
	}
	if reuse[1] {
		t.Error("the same copy cannot answer two entries of one name")
	}
}
