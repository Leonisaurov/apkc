package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// TestBuildStampNamesTheCommitOrSaysItCannot is the whole point of the command: a number
// in a log has to be attributable to a build, and when it cannot be, the tool has to say
// that instead of printing nothing or printing someone else's commit.
func TestBuildStampNamesTheCommitOrSaysItCannot(t *testing.T) {
	full := "8a3f1c0d2b4e6f9a1b2c3d4e5f60718293a4b5c6"

	clean := formatBuildStamp([]debug.BuildSetting{
		{Key: "vcs.revision", Value: full},
		{Key: "vcs.modified", Value: "false"},
		{Key: "vcs.time", Value: "2026-10-25T22:51:03Z"},
	}, "github.com/ajinasokan/apkc")
	if !strings.HasPrefix(clean, full[:12]) {
		t.Errorf("the stamp has to open with the first 12 bytes of the commit, it is %q", clean)
	}
	if strings.Contains(clean, "not committed") {
		t.Errorf("a build from a clean tree must not claim files are missing: %q", clean)
	}
	if !strings.Contains(clean, "2026-10-25") {
		t.Errorf("when the build was made is part of telling two binaries apart: %q", clean)
	}
	if strings.Contains(clean, full) {
		t.Error("the whole 40 byte revision does not belong in a log line")
	}

	dirty := formatBuildStamp([]debug.BuildSetting{
		{Key: "vcs.revision", Value: full},
		{Key: "vcs.modified", Value: "true"},
	}, "github.com/ajinasokan/apkc")
	if !strings.Contains(dirty, "with files not committed") {
		t.Errorf("a build from a dirty tree has to say so, or its numbers are unrepeatable: %q", dirty)
	}

	// the two ways a binary can fail to know its own commit
	fromModule := formatBuildStamp(nil, "github.com/ajinasokan/apkc")
	if fromModule != "no commit information in this build (github.com/ajinasokan/apkc)" {
		t.Errorf("a module built without a checkout must name the module: %q", fromModule)
	}
	if bare := formatBuildStamp(nil, ""); bare != "no commit information in this build" {
		t.Errorf("a binary that knows nothing about itself still has to answer one line: %q", bare)
	}
}

// TestBuildStampNeverAnswersEmpty because the caller prints it into a log: an empty value
// there is the failure the command exists to avoid.
func TestBuildStampNeverAnswersEmpty(t *testing.T) {
	if got := buildStamp(); got == "" {
		t.Error("buildStamp has to answer something, even when the build info is missing")
	}
}
