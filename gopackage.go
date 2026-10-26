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
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// signer choices. go is the one that keeps a round from starting two JVMs; jvm is the
// one with years of production behind it and the only one that reads a JKS keystore,
// assembles a bundle or signs a package the way a platform older than 4.3 reads one.
// signerDefault is what a build uses when the command line says nothing, and auto is
// the answer that lets the facts that decide the backend speak instead of a guess made
// once per project by whoever wrote the wrapper.
const (
	signerJVM     = "jvm"
	signerGo      = "go"
	signerAuto    = "auto"
	signerDefault = signerAuto
)

// minAPIForSHA256Digest is the level apksigner itself draws at when it decides whether
// the JAR signature has to carry SHA-1 digest pairs along with the SHA-256 ones: the
// platform can read a SHA-256 digest out of a manifest from 4.3 up, and below that it
// only knows SHA-1. What apkc writes is SHA-256, and a signature over an algorithm an
// absent device is the only witness of is not something a signing tool should guess at,
// so auto leaves a project of that age to the backend that has been shipping one.
const minAPIForSHA256Digest = 18

// resolveSigner turns the choice on the command line into the backend this build can
// actually use, and says why for the ones auto settles by itself. The three things it
// checks are the three limits of the Go packager: a bundle is assembled and signed by
// other tools, a JKS keystore is a format it refuses to read, and a package under API
// 18 is verified by a platform that cannot read the digests it writes.
//
// An explicit --signer=go is not second-guessed here: the user who names a backend gets
// that backend and its error, not a different one chosen behind their back.
func resolveSigner(signer string, api int, keyPath string, useAAB bool) string {
	if signer != signerAuto {
		return signer
	}
	switch {
	case useAAB:
		LogD("sign", "--signer=auto: a bundle is signed by jarsigner, so the jvm backend it is")
		return signerJVM
	case !keystoreIsPKCS12(keyPath):
		LogD("sign", "--signer=auto:", keyPath, "is not a PKCS#12 keystore, and only the jvm backend reads the rest")
		return signerJVM
	case api < minAPIForSHA256Digest:
		LogD("sign", "--signer=auto: minSdkVersion", api, "is old enough that its platform reads SHA-1 jar digests, which only apksigner writes")
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
	records, reused, digests, err := writePackageEntries(out, plan, reuse, previous, DataTable.EntrySha256)
	if err != nil {
		return 0, err
	}

	// The JAR signature is part of the package: its three files have to be inside
	// before the central directory is written, and before the signing block that
	// covers every byte up to it.
	signed, err := writeV1Entries(out, plan, digests, key, cert)
	if err != nil {
		return 0, err
	}
	records = append(records, signed...)

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
	DataTable.EntrySha256 = digests
	return reused, nil
}

// writeV1Entries appends META-INF/ANDROID.SF, META-INF/ANDROID.RSA and
// META-INF/MANIFEST.MF, in the order apksigner writes them, and returns their records
// for the central directory.
//
// A missing content digest is a failure rather than an omission: a manifest that
// covers fewer files than the package holds is a signature that says nothing about the
// rest of it, and it still verifies.
func writeV1Entries(dst *os.File, plan []bundleEntry, digests map[string]string,
	key *rsa.PrivateKey, cert *x509.Certificate) ([]packedRecord, error) {
	files, missing := v1FilesOf(plan, digests)
	if missing > 0 {
		return nil, fmt.Errorf("%d entries of the package had no content digest to write down", missing)
	}

	manifest := jarManifest(files)
	sf := signatureFile(manifest, []int{2})
	block, err := pkcs7Signature(sf, key, cert)
	if err != nil {
		return nil, err
	}

	// the signature files are written now, so their date is this build's: there is
	// no source file to be honest about, and a verifier does not compare them to one
	when := dosTime(time.Now())
	records := []packedRecord{}
	for _, item := range []struct {
		name    string
		content []byte
	}{
		{v1SFEntry, sf},
		{v1BlockEntry, block},
		{v1ManifestEntry, manifest},
	} {
		offset, err := dst.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		rec, err := writeMemoryEntry(dst, item.name, item.content, offset, when)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
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
