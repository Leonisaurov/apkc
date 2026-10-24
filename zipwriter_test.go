package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPaddingAfterLandsDataOnTheBoundary(t *testing.T) {
	cases := []struct {
		name      string
		nameLen   int
		offset    int64
		boundary  uint16
		wantExtra uint32
	}{
		{"no requirement costs no padding", 5, 3, alignNone, 0},
		{"a header ending mid-word is padded to four", 5, 0, alignStandard, 1},
		{"a header already on the boundary needs no padding", 6, 0, alignStandard, 0},
		{"a native library is padded to a page", 9, 4090, alignPage, uint32(4096 - ((4090 + 30 + 9) % 4096))},
	}
	for _, c := range cases {
		got := paddingAfter(c.nameLen, c.offset, c.boundary)
		if got != c.wantExtra {
			t.Errorf("%s: paddingAfter = %d, want %d", c.name, got, c.wantExtra)
		}
		if c.boundary > 1 && (c.offset+lfhHeader+int64(c.nameLen)+int64(got))%int64(c.boundary) != 0 {
			t.Errorf("%s: data would start at %d, not on a %d boundary", c.name,
				c.offset+lfhHeader+int64(c.nameLen)+int64(got), c.boundary)
		}
	}
}

func TestHeadersOfAWrittenEntryAgreeWithItsDirectoryRecord(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "payload")
	if err := os.WriteFile(src, []byte("some bytes for the entry\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := os.Create(filepath.Join(dir, "pkg.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	plan := []bundleEntry{{src: src, name: "assets/payload", compress: false, digest: "d1"}}
	records, reused, err := writePackageEntries(out, plan, []bool{false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reused != 0 {
		t.Fatalf("a cold package reused %d entries", reused)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(filepath.Join(dir, "pkg.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	cd, eocd := centralDirectory(records, uint32(len(blob)))

	// zipalign's checker compares the local header against the directory record for
	// every entry and says "header mismatch" when a field differs: the date being
	// written in one and left zero in the other is exactly how it reported the bug
	// this guards.
	localDate := binary.LittleEndian.Uint32(blob[10:14])
	dirDate := binary.LittleEndian.Uint32(cd[12:16])
	if localDate != dirDate {
		t.Errorf("dates disagree: local %08x directory %08x", localDate, dirDate)
	}
	for _, f := range []struct {
		label             string
		local, dir, width int
	}{
		{"method", 8, 10, 2},
		{"crc", lfhCrc, 16, 4},
		{"compressed size", lfhCSize, 20, 4},
		{"uncompressed size", lfhUSize, 24, 4},
	} {
		var a, b uint64
		if f.width == 2 {
			a = uint64(binary.LittleEndian.Uint16(blob[f.local:]))
			b = uint64(binary.LittleEndian.Uint16(cd[f.dir:]))
		} else {
			a = uint64(binary.LittleEndian.Uint32(blob[f.local:]))
			b = uint64(binary.LittleEndian.Uint32(cd[f.dir:]))
		}
		if a != b {
			t.Errorf("%s disagrees: local %d, directory %d", f.label, a, b)
		}
	}

	if got := binary.LittleEndian.Uint32(eocd[16:20]); got == 0 {
		t.Error("the end record must point at the central directory")
	}
	if len(cd) != 46+len("assets/payload") {
		t.Errorf("central directory is %d bytes, one record of 46 + the name expected", len(cd))
	}
	if binary.LittleEndian.Uint16(eocd[8:10]) != 1 {
		t.Error("the end record must count the one entry")
	}
}

func TestStoredEntryKeepsItsBytesAndCrc(t *testing.T) {
	dir := t.TempDir()
	body := []byte("an uncompressed payload that the loader may map")
	src := filepath.Join(dir, "big")
	if err := os.WriteFile(src, body, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "pkg"))
	if err != nil {
		t.Fatal(err)
	}

	plan := []bundleEntry{{src: src, name: "lib/arm64-v8a/libx.so", compress: false, digest: "d"}}
	records, _, err := writePackageEntries(out, plan, []bool{false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "pkg"))
	if err != nil {
		t.Fatal(err)
	}

	rec := records[0]
	if rec.csize != uint32(len(body)) || rec.usize != uint32(len(body)) {
		t.Errorf("a stored entry must claim the same compressed and uncompressed size, got %d and %d", rec.csize, rec.usize)
	}
	pad := paddingAfter(len(rec.name), rec.offset, alignPage)
	start := rec.offset + lfhHeader + int64(len(rec.name)) + int64(pad)
	if got := blob[start : start+int64(len(body))]; string(got) != string(body) {
		t.Error("the bytes in the package are not the bytes of the file")
	}
	if start%4096 != 0 {
		t.Errorf("a stored .so must start on a page, it starts at %d", start)
	}
}

func TestDosTimeStaysInsideTheFormatEpoch(t *testing.T) {
	if got := dosTime(time.Time{}); got != 0x210000 {
		t.Errorf("a zero time must land on the format epoch, got %08x", got)
	}
	if got := dosTime(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)); got != 0x210000 {
		t.Errorf("a date before 1980 must not wrap the year field, got %08x", got)
	}
	stamp := dosTime(time.Date(2026, 10, 24, 13, 45, 30, 0, time.UTC))
	// the date is the high half of the field, and the year within it is offset by
	// 1980: checking the wrong half is what failed here the first time
	if got := (stamp >> 16) >> 9 & 0x3f; got != 46 {
		t.Errorf("the year offset is %d (want 46) in %08x", got, stamp)
	}
	if got := stamp >> 16 & 0x1f; got != 24 {
		t.Errorf("the day is %d, want 24, in %08x", got, stamp)
	}
}

func TestIsNativeLibLooksAtTheFirstDirectory(t *testing.T) {
	for name, want := range map[string]bool{
		"lib/arm64-v8a/libx.so":     true,
		"arm64-v8a/libx.so":         false,
		"lib/arm64-v8a/libx.so.txt": false,
		"assets/notes.so":           false,
	} {
		if got := isNativeLib(name); got != want {
			t.Errorf("isNativeLib(%q) = %v, want %v", name, got, want)
		}
	}
}
