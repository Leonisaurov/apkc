package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// jksMagic is the header of a JKS keystore. PKCS#12 is an ASN.1 SEQUENCE, so its
// first byte is 0x30, and that is the whole difference a reader can see without a
// JVM: keytool wrote JKS until JDK 8 and PKCS#12 since, and both are still in
// use under ~/.android.
const jksMagic = uint32(0xFEEDFEED)

func isJKSHead(head []byte) bool {
	return len(head) >= 4 && binary.BigEndian.Uint32(head[:4]) == jksMagic
}

// signingKey is what the signer asks of the keystore: a key to sign with and the
// certificate that has to travel inside the signature block, so the platform can
// match the package against the signing identity on update.
func signingKey(path, password, alias string) (crypto.Signer, *x509.Certificate, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	// Both formats a keytool writes are read here: PKCS#12 since JDK 9, JKS before it,
	// and the two share nothing but the alias they name their entries with. Anything
	// else -- a JCEKS store, a file that is not a keystore at all -- is refused by name,
	// because guessing at a signing key is not a thing a signing tool does.
	reader := readPKCS12
	kind := "PKCS#12"
	if isJKSHead(blob) {
		reader, kind = readJKS, "JKS"
	}

	material, err := reader(blob, password, alias)
	if err != nil {
		return nil, nil, fmt.Errorf("%s (%s): %v", path, kind, err)
	}

	// An entry is named by the friendlyName its bag carries, not by the subject of
	// its certificate: the debug keystore is androiddebugkey by name and CN=Android
	// Debug by subject, so comparing the subject to the alias warned on every build
	// of every project. A file that does name its entries and does not hold the one
	// asked for is refused rather than answered with another identity: signing with a
	// key the caller did not choose installs once and then fails every update against
	// the old signature.
	if alias != "" && material.Alias != alias && (material.Names || material.Keys > 1) {
		return nil, nil, fmt.Errorf("%s holds %d key(s), the nearest name being %q, and none is %s: check --keyalias",
			path, material.Keys, material.Alias, alias)
	}
	LogD("sign", "keystore", path, "alias", material.Alias, "certificate", material.Cert.Subject.CommonName,
		"valid to", material.Cert.NotAfter.Format("2006-01-02"), "mac algorithm", material.MacAlgo)

	return material.Signer, material.Cert, nil
}

// keystoreIsReadable is what --signer=auto asks before committing a package to the Go
// signer: not "which format is it" but "is it one the readers in here open without a
// JVM". A JCEKS store, or a file that is not a keystore at all, is neither, and the
// refusal says so by the first four bytes rather than by a stack of parse errors.
func keystoreIsReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	return keystoreHeadIsReadable(head[:])
}

// keystoreHeadIsReadable is the same question on the four bytes themselves, so a test
// can ask it without a file on disk.
func keystoreHeadIsReadable(head []byte) bool {
	return isJKSHead(head) || (len(head) > 0 && head[0] == 0x30)
}

// requireKeystore stops a round that is about to sign with a path that holds nothing.
// Neither backend can do anything with a missing file, and neither says so in the terms
// the flag was written with: apksigner answers with a Java stack trace naming a path the
// user already typed, and the Go reader with a bare os error about opening it. This is
// not the format refusal of --signer=auto, which is about what apkc can read; a file that
// is not there is nobody's format, so it is fatal before the choice of backend matters.
func requireKeystore(path string) {
	if _, err := os.Stat(path); err != nil {
		LogF("sign", "there is no keystore at", path, "- no package can be signed without it.",
			"apkc doctor prints the path in use, and --keystore takes another one")
	}
}

// rsaKey narrows the signer to the algorithm a debug keystore uses, so an
// EC or Ed25519 key is refused by name instead of failing inside the format code
// with a signature nobody can read.
func rsaKey(signer crypto.Signer) (*rsa.PrivateKey, error) {
	key, ok := signer.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the Go signer writes RSA signatures; this keystore holds a %T", signer)
	}
	if key == nil || key.N == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("the key of the keystore is below 2048 bits, and an apk signature needs at least that")
	}
	return key, nil
}
