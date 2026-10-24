package main

// APK Signature Scheme v2, written to the format the platform verifies.
//
// What is signed is a digest over three of the four sections of the package: the
// local entries, the central directory and the end record. The digest is a two-level
// tree: each section is cut into 1 MB chunks, the digest of a chunk is over 0xa5,
// its length as uint32 little endian and its bytes, and the section digests are
// folded into one digest over 0x5a, the total number of chunks and every chunk
// digest in the order they appear. Chunks never cross a section boundary, which is
// what makes the whole thing computable in one pass over a file that can be
// hundreds of megabytes.
//
// The end record is hashed with its "offset of central directory" field replaced by
// the offset of the signing block, because that field changes every time the block
// changes size: without the substitution the signature could not cover the record it
// depends on.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	// the ID of the v2 block inside the APK Signing Block
	apkSigningBlockIDv2 = 0x7109871a
	// RSASSA-PKCS1-v1_5 with SHA-256, the algorithm a debug RSA key signs with
	sigAlgRSASSAWithSHA256 = 0x0103
	// the magic that lets a verifier find the block before the central directory
	apkSigningBlockMagic = "APK Sig Block 42"
	// the padding pair, which makes the block a whole number of pages so the
	// central directory that follows starts on one
	apkVerityPaddingBlockID = 0x42726577
	apkSigBlockPadding      = 4096

	contentChunkBytes = 1 << 20
)

// seqOf encodes a sequence of length-prefixed elements: every element carries its own
// uint32 length, and the sequence has no length of its own. The enclosing element is
// what gives the sequence its length, which is why a sequence inside a sequence needs
// no extra bookkeeping.
func seqOf(elements ...[]byte) []byte {
	out := []byte{}
	for _, e := range elements {
		out = appendUint32(out, uint32(len(e)))
		out = append(out, e...)
	}
	return out
}

// pairsOf encodes a sequence of (id, blob) pairs: uint32 of what follows, uint32 id,
// uint32 blob length, blob.
func pairsOf(items [][2]interface{}) []byte {
	out := []byte{}
	for _, item := range items {
		id := item[0].(uint32)
		blob := item[1].([]byte)
		out = appendUint32(out, uint32(8+len(blob)))
		out = appendUint32(out, id)
		out = appendUint32(out, uint32(len(blob)))
		out = append(out, blob...)
	}
	return out
}

// chunkedSHA256 is the content digest v2 stores. Each section is chunked on its own,
// so the boundaries of the package survive a chunk landing exactly on one.
func chunkedSHA256(sections ...sectionToDigest) ([]byte, error) {
	chunks := 0
	for _, s := range sections {
		if s.size > 0 {
			chunks += int((s.size + contentChunkBytes - 1) / contentChunkBytes)
		}
	}
	if chunks == 0 {
		return nil, fmt.Errorf("nothing to sign: the package has no sections")
	}

	// 5a | uint32 chunk count | every chunk digest, in file order
	root := make([]byte, 5+32*chunks)
	root[0] = 0x5a
	binary.LittleEndian.PutUint32(root[1:], uint32(chunks))

	buf := make([]byte, contentChunkBytes)
	at := 0
	for _, s := range sections {
		remaining := s.size
		for remaining > 0 {
			n := remaining
			if n > contentChunkBytes {
				n = contentChunkBytes
			}
			if _, err := s.r.ReadAt(buf[:n], s.size-remaining); err != nil && err != io.EOF {
				return nil, err
			}

			prefix := [5]byte{0xa5}
			binary.LittleEndian.PutUint32(prefix[1:], uint32(n))
			h := sha256.New()
			h.Write(prefix[:])
			h.Write(buf[:n])
			h.Sum(root[5+at*32 : 5+at*32])
			copy(root[5+at*32:], h.Sum(nil)[:])
			at++
			remaining -= n
		}
	}

	final := sha256.Sum256(root)
	return final[:], nil
}

// sectionToDigest is one of the parts of the package the digest covers.
type sectionToDigest struct {
	r    io.ReaderAt
	size int64
}

// v2SignerBlock builds the pair the signing block carries: signed data, the signature
// over it, and the public key the certificate must agree with.
func v2SignerBlock(digest []byte, key *rsa.PrivateKey, cert *x509.Certificate) ([]byte, error) {
	pub, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	digests := pairsOf([][2]interface{}{{uint32(sigAlgRSASSAWithSHA256), digest}})
	certs := seqOf(cert.Raw)
	// the fourth element of the signed data is the v1 signed data, empty because the
	// Go signer writes no JAR signature; the third is the additional attributes,
	// empty because v2 needs no stripping protection of its own
	signedData := seqOf(digests, certs, []byte{}, []byte{})

	// the signature is over the bytes of the signed data, not over the content
	// digest: the digest inside signed data is what covers the package, and this
	// signature is what covers that digest. Confusing the two produces a block that
	// parses and verifies against nothing.
	signedDigest := sha256.Sum256(signedData)
	rsaSignature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, signedDigest[:])
	if err != nil {
		return nil, err
	}
	signatures := pairsOf([][2]interface{}{{uint32(sigAlgRSASSAWithSHA256), rsaSignature}})

	// the public key is checked against the first certificate by the verifier, so it
	// is the one from the certificate, not a serialization of the signing key.
	//
	// Two wrappers, not one: the pair's value is a length-prefixed sequence of
	// length-prefixed signers, and a sequence is itself carried as a length-prefixed
	// element. Comparing against an apk apksigner signed with v2 only is what shows it
	// - with one wrapper the file parses as a signer of the wrong size and the
	// verifier answers "malformed signature record".
	signer := seqOf(signedData, signatures, pub)
	return seqOf(seqOf(signer)), nil
}

// apkSigningBlock wraps the v2 block in the container the platform looks for, and
// pads it to a multiple of the page size the way apksigner does: the central directory
// that follows then starts on a page boundary, which is what lets ART map it.
func apkSigningBlock(v2Block []byte) []byte {
	const fixed = 8 + 8 + 16 // the two size fields and the magic
	pairs := uint64(8 + 4 + len(v2Block))
	blockSize := uint64(fixed) + pairs

	padding := uint64(0)
	if rem := blockSize % apkSigBlockPadding; rem != 0 {
		padding = apkSigBlockPadding - rem
		if padding < 12 { // the smallest pair that can carry a size and an ID
			padding += apkSigBlockPadding
		}
	}
	pairs += padding
	blockSize = uint64(fixed) + pairs

	out := make([]byte, 0, blockSize+8)
	out = appendUint64(out, blockSize-8) // size of the block, excluding this field
	out = appendUint64(out, uint64(4+len(v2Block)))
	out = appendUint32(out, apkSigningBlockIDv2)
	out = append(out, v2Block...)
	if padding > 0 {
		out = appendUint64(out, padding-8)
		out = appendUint32(out, apkVerityPaddingBlockID)
		out = append(out, make([]byte, padding-12)...)
	}
	out = appendUint64(out, blockSize-8)
	return append(out, apkSigningBlockMagic...)
}

func appendUint32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendUint64(b []byte, v uint64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

// fileSection is a package region read straight from the file being written.
func fileSection(f *os.File, size int64) sectionToDigest {
	return sectionToDigest{r: f, size: size}
}

// bytesSection is a region already in memory, which is what the central directory and
// the end record are: small, and needed before they are written.
func bytesSection(b []byte) sectionToDigest {
	return sectionToDigest{r: readerAt(b), size: int64(len(b))}
}

type readerAt []byte

func (b readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
