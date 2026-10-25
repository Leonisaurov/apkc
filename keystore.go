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

	if isJKSHead(blob) {
		return nil, nil, fmt.Errorf("%s is a JKS keystore: the Go signer reads PKCS#12, convert it with keytool -importkeystore -storetype PKCS12 or build with --signer=jvm", path)
	}

	material, err := readPKCS12(blob, password, alias)
	if err != nil {
		return nil, nil, err
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

// keystoreIsPKCS12 answers what the Go signer can open without reading the key:
// PKCS#12 is an ASN.1 SEQUENCE, so its first byte is 0x30, while JKS starts with the
// magic below. The name inside the file is no help, keytool writes both formats with
// the same extension, and a debug keystore of one JDK is a PKCS#12 of the next.
func keystoreIsPKCS12(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	// a PKCS#12 file is an ASN.1 SEQUENCE: 0x30 in front and no JKS magic, which is
	// what the two formats have to tell each other apart by
	return head[0] == 0x30 && !isJKSHead(head[:])
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
