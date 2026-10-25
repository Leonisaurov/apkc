package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"
	"time"
)

// TestJarManifestIsWhatJavaWrites pins the serialization a verifier re-derives: the
// main section, then one section per file sorted by name, every line closed with CRLF
// and every section followed by a blank line.
func TestJarManifestIsWhatJavaWrites(t *testing.T) {
	manifest := string(jarManifest([]v1File{
		{name: "classes.dex", digest: "AAA="},
		{name: "AndroidManifest.xml", digest: "BBB="},
	}))

	want := "Manifest-Version: 1.0\r\n\r\n" +
		"Name: AndroidManifest.xml\r\nSHA-256-Digest: BBB=\r\n\r\n" +
		"Name: classes.dex\r\nSHA-256-Digest: AAA=\r\n\r\n"
	if manifest != want {
		t.Errorf("manifest bytes differ:\n got %q\nwant %q", manifest, want)
	}
}

// TestManifestFoldsLongValues is the part nobody guesses: java.util.jar writes the
// first 72 bytes of an attribute, then continuations of a space and up to 71 more. An
// asset path over that length is not exotic, and a verifier digests the section as
// re-serialised, so a wrong fold is a signature that does not verify on a device.
func TestManifestFoldsLongValues(t *testing.T) {
	long := strings.Repeat("a", 150)
	var b strings.Builder
	writeAttr(&b, attrName, long)
	got := b.String()

	if !strings.HasSuffix(got, "\r\n") {
		t.Errorf("the attribute must end with CRLF: %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n")
	if len(lines) < 3 {
		t.Fatalf("a 173 byte attribute must fold, got %d lines", len(lines))
	}
	if len(lines[0]) > 72 {
		t.Errorf("the first line is %d bytes, the limit is 72", len(lines[0]))
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, " ") {
			t.Errorf("a continuation must start with one space: %q", line)
		}
		if len(line) > 72 {
			t.Errorf("a continuation is %d bytes, the limit is 1 plus 71", len(line))
		}
	}
	// unfolding has to give the value back exactly, which is what a verifier does
	// before it compares digests
	rejoined := lines[0]
	for _, line := range lines[1:] {
		rejoined += line[1:]
	}
	if want := attrName + ": " + long; rejoined != want {
		t.Errorf("unfolding lost bytes: got %d, want %d", len(rejoined), len(want))
	}
}

func TestSignatureFileDigestsTheManifestItCovers(t *testing.T) {
	files := []v1File{
		{name: "classes.dex", digest: base64SHA256([]byte("dex"))},
		{name: "res/layout/main.xml", digest: base64SHA256([]byte("layout"))},
	}
	manifest := jarManifest(files)
	sf := string(signatureFile(manifest, []int{1, 2}))

	if !strings.HasPrefix(sf, "Signature-Version: 1.0\r\n") {
		t.Errorf("the .SF main section must start with the signature version: %q", sf[:40])
	}
	for _, want := range []string{
		attrCreatedBy + ": " + createdByValue + "\r\n",
		attrDigestMain + ": " + base64SHA256(manifest) + "\r\n",
		attrApkSigned + ": 1, 2\r\n",
	} {
		if !strings.Contains(sf, want) {
			t.Errorf("the .SF main section is missing %q", want)
		}
	}

	// each .SF section digests the manifest section as the manifest file holds it
	for _, section := range strings.SplitAfter(string(manifest), "\r\n\r\n")[1:] {
		if !strings.HasPrefix(section, attrName+": ") {
			continue // the trailing empty piece after the last section
		}
		name := strings.SplitN(strings.TrimPrefix(section, attrName+": "), "\r\n", 2)[0]
		want := base64SHA256([]byte(section))
		block, found := sfSection(t, sf, name)
		if !found {
			t.Fatalf("no .SF section for %q", name)
		}
		if !strings.Contains(block, attrDigest+": "+want) {
			t.Errorf("the section of %q digests to something else:\n got %q\nwant %s", name, block, want)
		}
	}
}

func sfSection(t *testing.T, sf, name string) (string, bool) {
	t.Helper()

	for _, section := range strings.Split(sf, "\r\n\r\n") {
		if strings.HasPrefix(section, attrName+": "+name+"\r\n") {
			return section, true
		}
	}
	return "", false
}

func TestV1FilesOfSkipsTheSignatureItself(t *testing.T) {
	plan := []bundleEntry{
		{name: "classes.dex", digest: "d1"},
		{name: "assets/blob.bin", digest: "d2"},
		{name: "META-INF/MANIFEST.MF", digest: "d3"},
		{name: "META-INF/ANDROID.SF", digest: "d4"},
	}
	files, missing := v1FilesOf(plan, map[string]string{
		"classes.dex":          "AAA=",
		"assets/blob.bin":      "BBB=",
		"META-INF/MANIFEST.MF": "CCC=",
		"META-INF/ANDROID.SF":  "DDD=",
	})
	if missing != 0 {
		t.Errorf("nothing should be missing here, got %d", missing)
	}
	if len(files) != 2 {
		t.Fatalf("the manifest must cover the two payload files, got %v", files)
	}
	for _, f := range files {
		if strings.HasPrefix(f.name, "META-INF/") {
			t.Errorf("the signature is inside its own manifest: %q", f.name)
		}
	}

	// an entry with no digest is counted, not silently left out: a manifest that
	// covers less than the package is a signature that verifies over nothing
	_, missing = v1FilesOf(plan, map[string]string{"classes.dex": "AAA="})
	if missing != 1 {
		t.Errorf("an entry without a digest must be reported, got %d", missing)
	}
}

// TestPKCS7SignatureVerifiesFromItsOwnBytes is the round a device takes: read the
// SignedData back, take the certificate out of it, and check the signature over the
// .SF with that certificate's key. A structure Go marshalled into something else
// cannot survive this, and neither can a signature over the wrong bytes.
func TestPKCS7SignatureVerifiesFromItsOwnBytes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(0x333BA584FF7A98C5),
		Subject:      pkix.Name{CommonName: "Android Debug", Organization: []string{"Android"}, Country: []string{"US"}},
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

	sf := []byte("Signature-Version: 1.0\r\n\r\n")
	block, err := pkcs7Signature(sf, key, cert)
	if err != nil {
		t.Fatal(err)
	}

	var outer pkcs7ContentInfo
	if _, err := asn1.Unmarshal(block, &outer); err != nil {
		t.Fatalf("the block is not a ContentInfo: %v", err)
	}
	// the digits are spelled out because a constant on both sides of the comparison
	// would agree with itself even when the number is wrong
	if !outer.ContentType.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}) {
		t.Errorf("content type = %v, want pkcs7-signedData (1.2.840.113549.1.7.2)", outer.ContentType)
	}

	var data pkcs7SignedData
	if _, err := asn1.Unmarshal(outer.Content.Bytes, &data); err != nil {
		t.Fatalf("the [0] wrapper does not hold a SignedData: %v", err)
	}
	if outer.Content.Class != 2 || outer.Content.Tag != 0 {
		t.Errorf("the SignedData must sit in a constructed context tag 0, got class %d tag %d",
			outer.Content.Class, outer.Content.Tag)
	}
	if !data.Content.ContentType.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}) {
		t.Errorf("the signed content must be pkcs7-data, got %v", data.Content.ContentType)
	}
	if data.Certificates.Class != 2 || data.Certificates.Tag != 0 {
		t.Errorf("the certificates must sit in a context tag 0, got class %d tag %d",
			data.Certificates.Class, data.Certificates.Tag)
	}
	if len(data.SignerInfos) != 1 {
		t.Fatalf("one signer was expected, got %d", len(data.SignerInfos))
	}
	signer := data.SignerInfos[0]
	if !signer.DigestAlgorithm.Algorithm.Equal(oidSHA256) {
		t.Errorf("digest algorithm = %v, want sha256", signer.DigestAlgorithm.Algorithm)
	}
	if !signer.DigestEncryptionAlgorithm.Algorithm.Equal(oidRSAEncryption) {
		t.Errorf("the signed digest must be rsaEncryption, got %v", signer.DigestEncryptionAlgorithm.Algorithm)
	}
	if signer.IssuerAndSerialNumber.SerialNumber.Int64() != template.SerialNumber.Int64() {
		t.Error("the signer does not name the certificate's serial")
	}

	parsed, err := x509.ParseCertificate(data.Certificates.Bytes)
	if err != nil {
		t.Fatalf("the certificate has to be readable straight out of [0]: %v", err)
	}
	if parsed.SerialNumber.Cmp(template.SerialNumber) != 0 {
		t.Error("a different certificate travelled in the block")
	}

	digest := sha256.Sum256(sf)
	if err := rsa.VerifyPKCS1v15(parsed.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], signer.EncryptedDigest); err != nil {
		t.Errorf("the signature does not verify over the .SF bytes: %v", err)
	}

	if _, err = pkcs7Signature(sf, nil, cert); err == nil {
		t.Error("a block was produced without a key")
	}
	if _, err = pkcs7Signature(sf, key, nil); err == nil {
		t.Error("a block was produced without a certificate")
	}
}
