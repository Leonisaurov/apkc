package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/asn1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// debugKeystore is the file apkc signs with by default. The reader is tested
// against it, and not against a fixture, because the whole reason it exists is
// that x/crypto/pkcs12 refuses what keytool writes today.
func debugKeystore(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to look for a debug keystore in")
	}
	path := filepath.Join(home, ".android", "debug.keystore")
	if !exists(path) {
		t.Skipf("no debug keystore at %s", path)
	}
	return path
}

func TestSigningKeyOpensTheDebugKeystore(t *testing.T) {
	path := debugKeystore(t)

	signer, cert, err := signingKey(path, "android", "androiddebugkey")
	if err != nil {
		t.Fatalf("the keystore apksigner uses was not readable: %v", err)
	}
	key, err := rsaKey(signer)
	if err != nil {
		t.Fatalf("the debug key is not something the Go signer can use: %v", err)
	}
	if key.N.BitLen() < 2048 {
		t.Errorf("debug key of %d bits, an apk signature needs at least 2048", key.N.BitLen())
	}
	if cert.SerialNumber == nil || len(cert.Raw) == 0 {
		t.Fatal("the certificate has no DER to put inside a signature block")
	}
	// the file's own MAC is reported for the log line, and this keystore's is
	// HMAC-SHA256, the very algorithm x/crypto/pkcs12 chokes on
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	material, err := readPKCS12(blob, "android")
	if err != nil {
		t.Fatalf("readPKCS12: %v", err)
	}
	if material.MacAlgo != "1.2.840.113549.2.9" && material.MacAlgo != "2.16.840.1.101.3.4.2.1" {
		t.Errorf("the MAC algorithm of the file was read as %q, want the SHA-256 one it carries", material.MacAlgo)
	}

	// the bags carry no alias to select by: the certificate is what says whose key
	// this is, and the debug key's common name is what apksigner prints
	if cert.Subject.CommonName != "Android Debug" {
		t.Errorf("certificate subject = %q, want the debug identity", cert.Subject.CommonName)
	}
}

func TestSigningKeyWrongPasswordIsNamedAsAPasswordProblem(t *testing.T) {
	path := debugKeystore(t)

	_, _, err := signingKey(path, "not the password", "androiddebugkey")
	if err == nil {
		t.Fatal("a wrong storepass opened the keystore")
	}
	if !strings.Contains(err.Error(), "storepass") {
		t.Errorf("the error must point at the password, got %v", err)
	}
}

func TestSigningKeyRefusesJKSInsteadOfMisreadingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.jks")

	// the JKS header followed by anything: stopping at the format is worth more
	// than a decode error that reads like a corrupt file
	blob := append([]byte{0xFE, 0xED, 0xFE, 0xED}, make([]byte, 64)...)
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}

	_, _, err := signingKey(path, "android", "androiddebugkey")
	if err == nil {
		t.Fatal("a JKS keystore was accepted by a PKCS#12 reader")
	}
	for _, want := range []string{"JKS", "--signer=jvm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q so the user knows what to do, got %v", want, err)
		}
	}
}

func TestSigningKeyMissingFileSaysWhich(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.p12")

	_, _, err := signingKey(path, "android", "androiddebugkey")
	if err == nil || !strings.Contains(err.Error(), "absent.p12") {
		t.Fatalf("the error must name the file that is not there, got %v", err)
	}
}

// TestDecryptPBERefusesAnOlderSchemeByName covers the other half of the decision:
// a keystore encrypted with the pre-PBKDF2 schemes is not decoded by guessing a
// second key derivation, it is refused with the name of what it is.
func TestDecryptPBERefusesAnOlderSchemeByName(t *testing.T) {
	_, err := decryptPBE(algoID{OID: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 8}}, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, "android")
	if err == nil {
		t.Fatal("pbeWithSHA1And3-KeyTripleDES was accepted by a PBES2-only reader")
	}
	for _, want := range []string{"1.2.840.113549.1.5.8", "--signer=jvm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must carry the OID and the way out, got %v", err)
		}
	}
}

func TestBMPStringIsThePasswordPKCS12DerivesFrom(t *testing.T) {
	got, err := bmpString("android")
	if err != nil {
		t.Fatal(err)
	}
	// UTF-16BE with a terminating zero: PBKDF2 over the bytes of the password as
	// PKCS#12 spells it, not as Go does
	if want := "0061006e00640072006f006900640000"; hex.EncodeToString(got) != want {
		t.Errorf("bmpString = %s, want %s", hex.EncodeToString(got), want)
	}

	if _, err := bmpString("a💀b"); err == nil {
		t.Error("a character outside the BMP must be refused, not truncated")
	}
}

func TestRSAKeyNamesTheAlgorithmItRefuses(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rsaKey(ec); err == nil || !strings.Contains(err.Error(), "*ecdsa.PrivateKey") {
		t.Errorf("an EC key must be refused by type, got %v", err)
	}

	if _, err := rsaKey(ed25519.NewKeyFromSeed(make([]byte, 32))); err == nil {
		t.Error("an Ed25519 key must not be accepted by an RSA signer")
	}

	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rsaKey(small); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Errorf("a 1024 bit key must be refused with its size, got %v", err)
	}
}
