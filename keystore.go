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

	material, err := readPKCS12(blob, password)
	if err != nil {
		return nil, nil, err
	}

	// the bags carry no alias to select by, so the certificate is checked against
	// what the caller asked for. A mismatch is reported rather than ignored:
	// signing with another key of the same keystore produces an apk that installs
	// once and then fails every update against the old signature.
	if alias != "" && material.Cert.Subject.CommonName != "" && material.Cert.Subject.CommonName != alias {
		LogW("sign", "the keystore offers", material.Cert.Subject.CommonName, "but", alias, "was asked for")
	}
	LogD("sign", "keystore", path, "certificate", material.Cert.Subject.CommonName,
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
