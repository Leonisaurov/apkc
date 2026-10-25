package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	material, err := readPKCS12(blob, "android", "androiddebugkey")
	if err != nil {
		t.Fatalf("readPKCS12: %v", err)
	}
	if material.MacAlgo != "1.2.840.113549.2.9" && material.MacAlgo != "2.16.840.1.101.3.4.2.1" {
		t.Errorf("the MAC algorithm of the file was read as %q, want the SHA-256 one it carries", material.MacAlgo)
	}

	// the alias lives in the friendlyName of the bags, and this keystore is the file
	// apkc signs with by default: reading it is what makes --keyalias mean anything
	if material.Alias != "androiddebugkey" {
		t.Errorf("entry name = %q, want the alias keytool wrote", material.Alias)
	}
	if material.Keys != 1 || !material.Names {
		t.Errorf("the file reported %d key(s), named = %v, want one named entry", material.Keys, material.Names)
	}

	// and the certificate keeps the identity the platform shows, which is not the
	// name of the entry
	if cert.Subject.CommonName != "Android Debug" {
		t.Errorf("certificate subject = %q, want the debug identity", cert.Subject.CommonName)
	}
}

// An alias that is not in the file must not be answered with whatever key is there:
// the apk would install once and refuse every later update against the old
// signature, which reads as a broken incremental build rather than as a wrong flag.
func TestSigningKeyRefusesAnAliasTheKeystoreDoesNotHold(t *testing.T) {
	path := debugKeystore(t)

	_, _, err := signingKey(path, "android", "release-key")
	if err == nil {
		t.Fatal("an alias this keystore does not carry was accepted")
	}
	for _, want := range []string{"release-key", "--keyalias"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q and the flag to fix, got %v", want, err)
		}
	}
}

// TestPickEntryAnswersTheAliasItIsGiven covers the two decisions the format leaves
// to apkc: which key answers an alias, and which certificate travels with it. The
// bags of a real keystore carry them in whatever order the writer used, so pairing
// by position would sign with one identity and ship another's certificate.
func TestPickEntryAnswersTheAliasItIsGiven(t *testing.T) {
	firstKey, firstCert := entryDER(t, "first", 1)
	secondKey, secondCert := entryDER(t, "second", 2)

	keys := []rawKey{firstKey, secondKey}
	certs := []rawCert{secondCert, firstCert}

	signer, cert, name, err := pickEntry(keys, certs, "second")
	if err != nil {
		t.Fatal(err)
	}
	if name != "second" {
		t.Errorf("chosen entry = %q, want the alias that was asked for", name)
	}
	if cert.SerialNumber == nil || cert.SerialNumber.Int64() != 2 {
		t.Errorf("certificate serial = %v, want the one whose key was chosen", cert.SerialNumber)
	}
	if public, err := x509.MarshalPKIXPublicKey(signer.Public()); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(public, cert.RawSubjectPublicKeyInfo) {
		t.Error("the key and the certificate returned are not the same entry")
	}

	// the shape keytool really writes: the name sits on the certificate bag and the
	// key bag carries only a localKeyID, so a reader that looked at the key alone
	// finds no alias in the files everyone uses
	unnamed := []rawKey{{der: firstKey.der}, {der: secondKey.der}}
	if _, cert, name, err := pickEntry(unnamed, certs, "second"); err != nil || name != "second" {
		t.Errorf("an alias written on the certificate must name its entry, got %q with %v", name, err)
	} else if cert.SerialNumber.Int64() != 2 {
		t.Errorf("certificate serial = %v, want the pair of the named entry", cert.SerialNumber)
	}

	// an alias that is not there falls back to the first key and says which, leaving
	// the accept-or-refuse call to a caller who knows the file name
	if _, _, name, err := pickEntry(keys, certs, "third"); err != nil || name != "first" {
		t.Errorf("an unknown alias must fall back to the first entry, got %q with %v", name, err)
	}

	if _, _, _, err := pickEntry(nil, certs, "first"); err == nil {
		t.Error("a keystore with no private key must be refused")
	}
	if _, _, _, err := pickEntry(keys, nil, "first"); err == nil {
		t.Error("a key with no certificate must be refused")
	}

	// a key none of the certificates carries is not answered by guessing which one
	// travels with it: that is the signature that verifies against the wrong identity
	_, strayCert := entryDER(t, "stray", 3)
	_, _, _, err = pickEntry([]rawKey{secondKey}, []rawCert{firstCert, strayCert}, "")
	if err == nil {
		t.Fatal("an unpaired key was answered with a certificate of another entry")
	}
	if !strings.Contains(err.Error(), "public key") {
		t.Errorf("the error must say what has no pair, got %v", err)
	}
}

// TestBagAliasReadsWhatEachWriterSpells covers the two spellings of the entry name:
// keytool writes it as a SET OF under a pkcs-9 id, openssl and the Windows tools as a
// bare string under the CSP id. A keystore is useless to --keyalias if the one the
// user has is the spelling that was not read.
func TestBagAliasReadsWhatEachWriterSpells(t *testing.T) {
	bmp, err := bmpString("release")
	if err != nil {
		t.Fatal(err)
	}
	name := asn1.RawValue{Tag: 30, Bytes: bmp}
	id := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 15}

	jdkName := mustMarshalAttr(t, oidFriendlyNameJDK, asn1.RawValue{Tag: 17, Bytes: mustEncodeRaw(t, name)})
	cspName := mustMarshalAttr(t, oidFriendlyNameCSP, name)

	if got := bagAlias(jdkName); got != "release" {
		t.Errorf("the keytool spelling read as %q", got)
	}
	if got := bagAlias(cspName); got != "release" {
		t.Errorf("the openssl spelling read as %q", got)
	}
	// a UTF8String name is a name too, not an unnamed bag
	if got := bagAlias(mustMarshalAttr(t, oidFriendlyNameCSP, asn1.RawValue{Tag: 12, Bytes: []byte("other")})); got != "other" {
		t.Errorf("a UTF8String friendlyName read as %q", got)
	}

	// the order of the attributes is the writer's, so neither of the two may be
	// assumed to come first
	withID := mustEncodeRaw(t, asn1.RawValue{Tag: 4, Bytes: []byte("Time 1788757390230")})
	keyID := mustMarshalAttr(t, id, asn1.RawValue{Tag: 17, Bytes: withID})
	if got := bagAlias(append(jdkName, keyID...)); got != "release" {
		t.Errorf("a bag whose name comes before its localKeyID read as %q", got)
	}
	if got := bagAlias(append(keyID, jdkName...)); got != "release" {
		t.Errorf("a bag whose name comes after its localKeyID read as %q", got)
	}

	// a bag that carries only an id, and a bag with nothing at all, are both unnamed
	if got := bagAlias(keyID); got != "" {
		t.Errorf("a localKeyID was read as an alias: %q", got)
	}
	if got := bagAlias(nil); got != "" {
		t.Errorf("no attributes read as %q, want the empty name", got)
	}
	// a truncated bag is unnamed, not a failure inside the signer
	if got := bagAlias([]byte{0x30, 0x03, 0x06}); got != "" {
		t.Errorf("a truncated attribute read as %q", got)
	}
}

// mustMarshalAttr builds one Attribute SEQUENCE of the bag attributes.
func mustMarshalAttr(t *testing.T, oid asn1.ObjectIdentifier, val asn1.RawValue) []byte {
	t.Helper()

	raw, err := asn1.Marshal(struct {
		OID asn1.ObjectIdentifier
		Val asn1.RawValue
	}{oid, val})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// mustEncodeRaw writes the full TLV of a value, which is how the attributes nest.
func mustEncodeRaw(t *testing.T, val asn1.RawValue) []byte {
	t.Helper()

	raw, err := asn1.Marshal(val)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// entryDER makes the two halves of a keystore entry in memory. The shape of the
// bags is not apkc's decision; which key answers which alias, and which certificate
// travels with it, is.
func entryDER(t *testing.T, alias string, serial int64) (rawKey, rawCert) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: alias},
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	return rawKey{alias, pkcs8}, rawCert{alias, cert}
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

// TestResolveSigner is the whole of --signer=auto: what the packager can read and what
// it can build decide it, and an explicit choice is never overruled. A JKS keystore is
// the case that matters, because the other backend is the only way to use one.
func TestResolveSigner(t *testing.T) {
	dir := t.TempDir()
	p12 := filepath.Join(dir, "release.p12")
	jks := filepath.Join(dir, "old.jks")
	for path, head := range map[string][]byte{p12: {0x30, 0x82, 0x01, 0x02}, jks: {0xFE, 0xED, 0xFE, 0xED}} {
		if err := os.WriteFile(path, append(head, make([]byte, 16)...), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		signer string
		key    string
		aab    bool
		want   string
	}{
		{"an explicit jvm stays", signerJVM, p12, false, signerJVM},
		{"an explicit go is not second guessed", signerGo, jks, false, signerGo},
		{"auto takes the go path over a PKCS#12 keystore", signerAuto, p12, false, signerGo},
		{"auto refuses a JKS keystore", signerAuto, jks, false, signerJVM},
		{"auto refuses a keystore that is not there", signerAuto, filepath.Join(dir, "absent.p12"), false, signerJVM},
		{"auto leaves a bundle to jarsigner", signerAuto, p12, true, signerJVM},
	}
	for _, c := range cases {
		if got := resolveSigner(c.signer, c.key, c.aab); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// the two formats have to be told apart by what a reader can see without a JVM,
	// because both arrive with the same extension and under the same name
	if !keystoreIsPKCS12(p12) {
		t.Error("a PKCS#12 header was not recognised")
	}
	if keystoreIsPKCS12(jks) {
		t.Error("a JKS header was read as PKCS#12")
	}
	if keystoreIsPKCS12(filepath.Join(dir, "absent.p12")) {
		t.Error("a file that is not there cannot be a keystore format")
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
