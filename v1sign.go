package main

// v1, the JAR signature, written here instead of by jarsigner and apksigner.
//
// The scheme is old enough that everything about it is fixed by java.util.jar: three
// files inside the package, META-INF/ANDROID.SF, META-INF/ANDROID.RSA and
// META-INF/MANIFEST.MF, in that order, which is the order apksigner puts them in and
// the order a verifier reads them in. The manifest carries one section per packaged
// file with the SHA-256 of its *uncompressed* bytes; the .SF carries one section per
// manifest section with the SHA-256 of those bytes, and a main attribute with the
// SHA-256 of the whole manifest; the .RSA is a PKCS#7 SignedData whose signerInfo
// carries an RSASSA-PKCS1-v1_5 signature over exactly the bytes of the .SF, with no
// authenticated attributes — which is what this file learned by taking a signature
// apksigner produced, pulling the encrypted digest out of it and having openssl
// verify it against the .SF.
//
// The serialization is not a detail. A verifier parses the manifest into Attributes
// and digests the *section as it would write it back*, so the line endings, the order
// of the sections and the way a long value folds onto continuation lines all have to
// be what java.util.jar produces. The folding is the part nobody guesses right: a
// line is written up to 72 bytes, and every continuation carries one leading space
// and up to 71 more bytes. That is the rule in Attributes.write, and it is reproduced
// here because an asset path longer than 72 bytes is not exotic.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"strings"
)

// the three names the signature lives under, and the attribute names of the format.
const (
	v1SignerName      = "ANDROID"
	v1ManifestEntry   = "META-INF/MANIFEST.MF"
	v1SFEntry         = "META-INF/" + v1SignerName + ".SF"
	v1BlockEntry      = "META-INF/" + v1SignerName + ".RSA"
	attrManifestVer   = "Manifest-Version"
	attrSignatureVer  = "Signature-Version"
	attrCreatedBy     = "Created-By"
	attrDigest        = "SHA-256-Digest"
	attrDigestMain    = "SHA-256-Digest-Manifest"
	attrApkSigned     = "X-Android-APK-Signed"
	attrName          = "Name"
	createdByValue    = "1.0 (Android)"
	manifestLineLimit = 72
	manifestFoldLimit = 71
)

// v1File is one packaged file the signature has to cover.
type v1File struct {
	name   string
	digest string // base64 of the SHA-256 of the bytes as they are stored inside the entry
}

// jarManifest is META-INF/MANIFEST.MF: the main section, then one section per file in
// the order a verifier expects, which is the name sorted the way java.util.jar sorted
// it when apksigner wrote it.
func jarManifest(files []v1File) []byte {
	var b strings.Builder
	writeAttr(&b, attrManifestVer, "1.0")
	b.WriteString("\r\n")

	for _, f := range sortedFiles(files) {
		writeAttr(&b, attrName, f.name)
		writeAttr(&b, attrDigest, f.digest)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// signatureFile is META-INF/ANDROID.SF. Its own bytes are what gets signed, so the
// manifest digest has to be in it before the signature is made, and nothing that
// comes after it may change them.
func signatureFile(manifest []byte, schemes []int) []byte {
	var b strings.Builder
	writeAttr(&b, attrSignatureVer, "1.0")
	writeAttr(&b, attrCreatedBy, createdByValue)
	writeAttr(&b, attrDigestMain, base64SHA256(manifest))
	writeAttr(&b, attrApkSigned, schemeList(schemes))
	b.WriteString("\r\n")

	// one section per manifest section, digesting the section exactly as the manifest
	// file holds it, trailing blank line included
	sections := strings.SplitAfter(string(manifest), "\r\n\r\n")
	for _, section := range sections[1:] {
		if !strings.HasPrefix(section, attrName+":") {
			continue
		}
		writeAttr(&b, attrName, attrValue(section, attrName))
		writeAttr(&b, attrDigest, base64SHA256([]byte(section)))
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// attrValue reads one attribute back out of a manifest section, joining the continuation
// lines a folded value left behind. The signature file needs the whole of a name: taking
// only the first physical line of a long one writes an .SF section for an entry that does
// not exist and no section at all for the one that does. apksigner does not call that a
// failure — it verifies what the .SF lists — and on the Firebase demo, whose drawable
// names run past the 72 byte limit, four of its entries shipped outside the signature.
func attrValue(section, attr string) string {
	lines := strings.Split(section, "\r\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], attr+": ") {
		return ""
	}
	value := lines[0][len(attr)+2:]
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, " ") {
			break
		}
		value += line[1:]
	}
	return value
}

// writeAttr writes one attribute the way java.util.jar does, folding any line past
// the first 72 bytes into continuations of 71 with a leading space.
func writeAttr(b *strings.Builder, name, value string) {
	line := name + ": " + value
	for len(line) > manifestLineLimit {
		b.WriteString(line[:manifestLineLimit])
		b.WriteString("\r\n")
		line = " " + line[manifestLineLimit:]
		if len(line) <= manifestFoldLimit+1 {
			break
		}
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}

func schemeList(schemes []int) string {
	parts := make([]string, 0, len(schemes))
	for _, s := range schemes {
		parts = append(parts, fmt.Sprintf("%d", s))
	}
	return strings.Join(parts, ", ")
}

func sortedFiles(files []v1File) []v1File {
	out := append([]v1File{}, files...)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func base64SHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// sha256File hashes one file of the project into the attribute form the manifest
// wants. It is the fallback of the digest cache: the packager normally carries the
// digest of everything it writes, and this runs only for an entry it copied over
// before the cache existed.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(digest.Sum(nil)), nil
}

// The OIDs of PKCS#7 and of the two algorithms a debug key uses. pkcs7-signedData is
// 1.2.840.113549.1.7.2 and not anything under 1.5.*: the PKCS arc sits inside rsadsi,
// and a wrong number here reads to a verifier as an unsupported content type. Nothing here is a
// choice: a verifier that does not recognise the identifier says "invalid signature"
// rather than "unsupported", so the numbers come from the signature apksigner writes.
var (
	oidPKCS7SignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidPKCS7Data       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSHA256          = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidRSAEncryption   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
)

// the structures of RFC 2315, as far as a detached signature over one file needs them.
type pkcs7ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"optional"`
}

type pkcs7IssuerAndSerial struct {
	Issuer       asn1.RawValue
	SerialNumber *big.Int
}

type pkcs7SignerInfo struct {
	Version                   int
	IssuerAndSerialNumber     pkcs7IssuerAndSerial
	DigestAlgorithm           pkix.AlgorithmIdentifier
	DigestEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedDigest           []byte
}

type pkcs7SignedData struct {
	Version          int
	DigestAlgorithms []pkix.AlgorithmIdentifier `asn1:"set"`
	Content          pkcs7ContentInfo
	Certificates     asn1.RawValue
	SignerInfos      []pkcs7SignerInfo `asn1:"set"`
}

// pkcs7Signature is the content of META-INF/ANDROID.RSA: a SignedData that carries the
// signing certificate and signs content with it, and nothing else. The signature is
// detached — the contentInfo names pkcs7-data and holds no bytes — because the .SF
// file is next to it inside the package, and the verifier reads it from there.
func pkcs7Signature(content []byte, key *rsa.PrivateKey, cert *x509.Certificate) ([]byte, error) {
	if key == nil || cert == nil {
		return nil, errors.New("the signature needs a key and the certificate that goes with it")
	}
	if len(cert.RawIssuer) == 0 || cert.SerialNumber == nil || len(cert.Raw) == 0 {
		return nil, errors.New("the certificate of the keystore carries no issuer, serial or DER")
	}

	digest := sha256.Sum256(content)
	signed, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, fmt.Errorf("the signing key refused the digest: %w", err)
	}

	data := pkcs7SignedData{
		Version:          1,
		DigestAlgorithms: []pkix.AlgorithmIdentifier{{Algorithm: oidSHA256, Parameters: asn1.NullRawValue}},
		Content:          pkcs7ContentInfo{ContentType: oidPKCS7Data},
		// [0] IMPLICIT SET OF Certificate: the context tag stands in for the SET, so
		// the bytes of the certificate follow it directly. Written with a universal
		// tag instead, apksig reads the structure as unsupported and says nothing more
		// useful than "failed to parse signerInfos".
		Certificates: asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: cert.Raw},
		SignerInfos: []pkcs7SignerInfo{{
			Version: 1,
			IssuerAndSerialNumber: pkcs7IssuerAndSerial{
				Issuer:       asn1.RawValue{FullBytes: cert.RawIssuer},
				SerialNumber: cert.SerialNumber,
			},
			DigestAlgorithm:           pkix.AlgorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue},
			DigestEncryptionAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidRSAEncryption, Parameters: asn1.NullRawValue},
			EncryptedDigest:           signed,
		}},
	}
	signedData, err := asn1.Marshal(data)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(pkcs7ContentInfo{
		ContentType: oidPKCS7SignedData,
		Content:     asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: signedData},
	})
}

// v1FilesOf is the manifest of a package: the name and the content digest of every
// entry the plan put in it. missing says how many entries had no digest to be handed
// over, which is what the caller refuses rather than write a manifest that lies.
func v1FilesOf(plan []bundleEntry, digests map[string]string) ([]v1File, int) {
	files := []v1File{}
	missing := 0
	for _, e := range plan {
		if strings.HasPrefix(e.name, "META-INF/") {
			// the signature does not cover itself: apksigner's manifest has no
			// META-INF section either
			continue
		}
		digest, ok := digests[e.name]
		if !ok || digest == "" {
			missing++
			continue
		}
		files = append(files, v1File{name: e.name, digest: digest})
	}
	return files, missing
}
