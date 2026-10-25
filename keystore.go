package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// jksMagic is the header of a JKS keystore. PKCS#12 is an ASN.1 SEQUENCE, so its
// first byte is 0x30, and that is the whole difference a reader can see without a
// JVM: keytool wrote JKS until JDK 8 and PKCS#12 since, and both are still in
// use under ~/.android.
const jksMagic = uint32(0xFEEDFEED)

// signingKey is what the signer asks of the keystore: a key to sign with and the
// certificate that has to travel inside the signature block, so the platform can
// match the package against the signing identity on update.
func signingKey(path, password, alias string) (crypto.Signer, *x509.Certificate, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	if len(blob) >= 4 && uint32(blob[0])<<24|uint32(blob[1])<<16|uint32(blob[2])<<8|uint32(blob[3]) == jksMagic {
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
