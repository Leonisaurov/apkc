package main

// The Go packager: one pass from the plan to an aligned, v2-signed apk, which is
// where zipalign and apksigner leave the build.
//
// Both are JVM starts that rewrite the whole package, and on the 60 jar demo the pair
// costs more than every other stage of an incremental round. Here the alignment is
// decided as each entry is written and the signature is computed over the sections on
// the way, so nothing is read back to be moved.
//
// What is deliberately not here: JAR signing (v1). A package signed only with v2 is
// verified by Android 7 and newer, so the packager refuses a project whose api level
// is below that rather than produce an apk that installs on nothing.

import (
	"archive/zip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// signer choices. jvm is the default and the one with years of production behind it;
// go is the new path; auto lets the two facts that decide whether go is allowed speak
// for themselves, which is what a wrapper passes once instead of thinking per project.
const (
	signerJVM  = "jvm"
	signerGo   = "go"
	signerAuto = "auto"

	// v2 is checked by the platform from API 24 up
	minAPIForV2Only = 24
)

// resolveSigner turns the choice on the command line into the backend this build can
// actually use, and says why for the ones auto settles by itself. The two conditions
// are the two limits of the Go packager: a package under API 24 needs the JAR
// signature it does not write, and a JKS keystore is a format it refuses to read.
//
// An explicit --signer=go is not second-guessed here: v2RefuseBelow is what tells the
// user their chosen backend cannot serve the project.
func resolveSigner(signer string, api int, keyPath string, useAAB bool) string {
	if signer != signerAuto {
		return signer
	}
	switch {
	case useAAB:
		LogD("sign", "--signer=auto: a bundle is signed by jarsigner, so the jvm backend it is")
		return signerJVM
	case api < minAPIForV2Only:
		LogD("sign", "--signer=auto: minSdkVersion", api, "needs the v1 signature only apksigner writes")
		return signerJVM
	case !keystoreIsPKCS12(keyPath):
		LogD("sign", "--signer=auto:", keyPath, "is not a PKCS#12 keystore, and only the jvm backend reads the rest")
		return signerJVM
	}
	LogD("sign", "--signer=auto: go signs this package, no zipalign and no apksigner")
	return signerGo
}

// packageWithGo writes build/app.apk from the plan. It reports how many entries came
// over from the previous package unchanged, which is the number that shows whether the
// reuse is doing its job.
func packageWithGo(plan []bundleEntry, recorded map[string]string, keyPath, storePass, keyAlias string) (int, error) {
	packed := filepath.Join("build", "app.apk")
	previous, err := openPreviousPackage(packed)
	if err != nil {
		return 0, err
	}
	defer previous.Close()

	signer, cert, err := signingKey(keyPath, storePass, keyAlias)
	if err != nil {
		return 0, err
	}
	key, err := rsaKey(signer)
	if err != nil {
		return 0, err
	}

	out, err := os.Create(packed + ".tmp")
	if err != nil {
		return 0, err
	}
	defer out.Close()

	reuse, entries := reusePlan(plan, previous.names, recorded)
	records, reused, err := writePackageEntries(out, plan, reuse, previous)
	if err != nil {
		return 0, err
	}

	// the end record is hashed with the offset of the signing block in the field that
	// will finally hold the offset of the central directory: that value changes every
	// time the block changes size, so without the substitution the signature could not
	// cover the record it depends on
	blockOffset, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	cd, eocd := centralDirectory(records, uint32(blockOffset))

	digest, err := chunkedSHA256(fileSection(out, blockOffset), bytesSection(cd), bytesSection(eocd))
	if err != nil {
		return 0, err
	}
	v2Block, err := v2SignerBlock(digest, key, cert)
	if err != nil {
		return 0, err
	}
	block := apkSigningBlock(v2Block)

	if _, err := out.Write(block); err != nil {
		return 0, err
	}
	binary.LittleEndian.PutUint32(eocd[16:], uint32(blockOffset+int64(len(block))))
	if _, err := out.Write(cd); err != nil {
		return 0, err
	}
	if _, err := out.Write(eocd); err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	// the rename replaces the file the reuse was read from: every byte this build
	// needed of it is already copied, and the descriptor keeps the old inode until the
	// deferred close runs
	if err := os.Rename(packed+".tmp", packed); err != nil {
		return 0, err
	}
	// the record of what the next build can copy over, kept here so the caller stores
	// it only when the package really landed
	DataTable.BundleEntries = entries
	return reused, nil
}

// openPreviousPackage indexes the package this one replaces: the offsets and the
// claims of every entry, which is all the reuse needs. Its absence is not a failure,
// it is a cold build, and an unreadable one costs the reuse and nothing else.
func openPreviousPackage(path string) (*previousPackage, error) {
	out := &previousPackage{index: map[string]*prevEntry{}, names: map[string]bool{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return out, err
	}
	reader, err := zip.NewReader(f, info.Size())
	if err != nil {
		// a package this build cannot read only matters through what it cannot
		// reuse: every entry is written from its source, as a cold bundle does
		LogW("build", "the package on disk cannot be read, writing every entry again", err.Error())
		f.Close()
		return out, nil
	}

	for _, file := range reader.File {
		at, err := file.DataOffset()
		if err != nil {
			LogW("build", "entry cannot be copied raw, it will be written again", file.Name, err.Error())
			continue
		}
		out.index[file.Name] = &prevEntry{
			dataAt:   at,
			dataLen:  uint32(file.CompressedSize64),
			usize:    uint32(file.UncompressedSize64),
			crc:      file.CRC32,
			deflate:  file.Method == zip.Deflate,
			dateTime: dosTime(file.Modified),
		}
		out.names[file.Name] = true
	}
	out.file = f
	return out, nil
}

// v2RefuseBelow is the check that keeps the packager from producing an apk no device
// of the project's own api level can install.
func v2RefuseBelow(api int) error {
	if api > 0 && api < minAPIForV2Only {
		return fmt.Errorf("--signer=go signs with APK Signature Scheme v2 only, which Android checks from API %d up: this project has minSdkVersion %d, build it with --signer=jvm", minAPIForV2Only, api)
	}
	return nil
}
