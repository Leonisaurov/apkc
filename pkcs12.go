package main

// A PKCS#12 reader for the one thing apkc needs from a keystore: the private key
// and its certificate, without starting a JVM.
//
// golang.org/x/crypto/pkcs12 cannot open a modern debug keystore: its MAC code
// knows SHA-1 only, and keytool has written HMAC-SHA256 since JDK 17, so the
// whole file is refused with "unknown digest algorithm" before the key is ever
// looked at. The MAC is what verifies the password, not what protects the key,
// so this reader does the second part and not the first: it decrypts the bags
// and reports the MAC algorithm it saw, and a wrong storepass still surfaces as
// a failure, at the PKCS#8 parse.
//
// It covers PBES2 with PBKDF2 and AES-CBC, which is what keytool produces with a
// PKCS#12 store. The older pbeWithSHA1And... schemes are named and refused, not
// guessed at: implementing a second key derivation nobody can test here is how a
// signing tool starts producing signatures that verify against the wrong key.

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// OIDs the format uses, as far as this reader goes.
var (
	oidData        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidEncrypted   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 6}
	oidKeyBag      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 1}
	oidShroudedBag = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidCertBag     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 3}
	oidPBES2       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACSHA1    = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACSHA256  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACSHA512  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
)

type algoID struct {
	OID  asn1.ObjectIdentifier
	Para asn1.RawValue
}

type contentInfo struct {
	OID     asn1.ObjectIdentifier
	Content asn1.RawValue `asn1:"tag:0,explicit,optional"`
}

// pfx reads the three elements of a PKCS#12 file. The last one is taken with rest
// rather than optional: Go's asn1 does not fill an optional RawValue, and a reader
// that silently loses the MacData would report no MAC algorithm at all.
type pfx struct {
	Version int
	Auth    contentInfo
	Mac     asn1.RawValue `asn1:"rest"`
}

// encryptedData is the payload of an encryptedData ContentInfo: a version and the
// EncryptedContentInfo. keytool writes the version INTEGER first, so a reader that
// flattens the sequence reads 0 as an OBJECT IDENTIFIER and fails with a complaint
// about a file that is perfectly well formed.
type encryptedData struct {
	Version int
	Info    encryptedContentInfo
}

type encryptedContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Encrypt     algoID
	Encrypted   asn1.RawValue `asn1:"tag:0,optional"`
}

type safeBag struct {
	OID   asn1.ObjectIdentifier
	Bag   asn1.RawValue   `asn1:"tag:0,explicit"`
	Attrs []asn1.RawValue `asn1:"optional,set"`
}

type certBag struct {
	OID  asn1.ObjectIdentifier
	Data asn1.RawValue `asn1:"tag:0,explicit"`
}

type pbeParams struct {
	Salt       []byte
	Iterations int
	KeyLength  int `asn1:"optional"`
	PRF        algoID
}

type pbes2Params struct {
	KDF struct {
		OID    asn1.ObjectIdentifier
		Params pbeParams
	}
	Cipher struct {
		OID      asn1.ObjectIdentifier
		IVParams []byte
	}
}

// keyMaterial is what a caller of this file wants out of a keystore.
type keyMaterial struct {
	Signer crypto.Signer
	Cert   *x509.Certificate
	// MacAlgo is reported, not verified: the OID of the digest the file's MAC was
	// computed with. A user who converts a keystore and loses the key in the
	// process at least sees which scheme the file claimed.
	MacAlgo string
}

// readPKCS12 opens a PKCS#12 blob with the store password.
func readPKCS12(blob []byte, password string) (keyMaterial, error) {
	var file pfx
	if _, err := asn1.Unmarshal(blob, &file); err != nil {
		return keyMaterial{}, fmt.Errorf("not a PKCS#12 file: %v", err)
	}

	mac := "none"
	// MacData.mac is a DigestInfo, so the AlgorithmIdentifier is one sequence
	// deeper: read flat, it matches nothing and the reader reports no MAC for a file
	// that clearly has one.
	var macInfo struct {
		Mac struct {
			Algo   algoID
			Digest []byte
		}
		Key   []byte
		Iters int `asn1:"default:1,optional"`
	}
	if _, err := asn1.Unmarshal(file.Mac.FullBytes, &macInfo); err == nil && macInfo.Mac.Algo.OID.String() != "" {
		mac = macInfo.Mac.Algo.OID.String()
	} else if err != nil {
		LogD("sign", "the keystore MAC was not read, and is not verified either", err.Error())
	}

	authSafe, err := octetStringOf(file.Auth.Content.Bytes)
	if err != nil {
		return keyMaterial{}, err
	}
	var infos []contentInfo
	if _, err := asn1.Unmarshal(authSafe, &infos); err != nil {
		return keyMaterial{}, fmt.Errorf("PKCS#12 AuthSafe: %v", err)
	}

	out := keyMaterial{MacAlgo: mac}
	bags := []safeBag{}
	for _, ci := range infos {
		var payload []byte
		switch {
		case ci.OID.Equal(oidData):
			if payload, err = octetStringOf(ci.Content.Bytes); err != nil {
				return out, err
			}
		case ci.OID.Equal(oidEncrypted):
			var ed encryptedData
			if _, err := asn1.Unmarshal(ci.Content.Bytes, &ed); err != nil {
				return out, fmt.Errorf("PKCS#12 encrypted content: %v", err)
			}
			if payload, err = decryptPBE(ed.Info.Encrypt, ed.Info.Encrypted.Bytes, password); err != nil {
				return out, err
			}
		default:
			continue
		}

		var inner []asn1.RawValue
		if _, err := asn1.Unmarshal(payload, &inner); err != nil {
			return out, fmt.Errorf("PKCS#12 SafeContents: %v", err)
		}
		for _, raw := range inner {
			var bag safeBag
			if _, err := asn1.Unmarshal(raw.FullBytes, &bag); err != nil {
				return out, fmt.Errorf("PKCS#12 SafeBag: %v", err)
			}
			bags = append(bags, bag)
		}
	}

	for _, bag := range bags {
		switch {
		case bag.OID.Equal(oidShroudedBag):
			var epki struct {
				Algo algoID
				Data asn1.RawValue
			}
			if _, err := asn1.Unmarshal(bag.Bag.Bytes, &epki); err != nil {
				return out, fmt.Errorf("PKCS#12 shrouded key bag: %v", err)
			}
			plaintext, err := decryptPBE(epki.Algo, epki.Data.Bytes, password)
			if err != nil {
				return out, err
			}
			if err := out.takeKey(plaintext); err != nil {
				return out, err
			}
		case bag.OID.Equal(oidKeyBag):
			if err := out.takeKey(bag.Bag.Bytes); err != nil {
				return out, err
			}
		case bag.OID.Equal(oidCertBag):
			var cb certBag
			if _, err := asn1.Unmarshal(bag.Bag.Bytes, &cb); err != nil {
				return out, fmt.Errorf("PKCS#12 certificate bag: %v", err)
			}
			der, err := octetStringOf(cb.Data.Bytes)
			if err != nil {
				return out, err
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				// the certificate id names what kind of bag this was: a compressed
				// or CRL bag lands here too, and naming it is worth more than a
				// parse error that reads like a corrupt keystore
				return out, fmt.Errorf("bag of certificate type %s is not a parsable X.509 certificate: %v", cb.OID, err)
			}
			if out.Cert == nil {
				out.Cert = cert
			}
		}
	}

	if out.Signer == nil {
		return out, errors.New("the keystore holds no private key apkc can use")
	}
	if out.Cert == nil {
		return out, errors.New("the keystore holds a key with no certificate")
	}
	return out, nil
}

// takeKey reads a PKCS#8 PrivateKeyInfo, which is what both the plain and the
// shrouded key bag carry once the bag is open.
func (k *keyMaterial) takeKey(der []byte) error {
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		// a wrong storepass most often surfaces here, because CBC padding on a
		// random key decodes to garbage far more often than to a parseable key
		return fmt.Errorf("private key in the keystore could not be read (wrong storepass?): %v", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return fmt.Errorf("the keystore holds a %T, which cannot sign an apk", key)
	}
	k.Signer = signer
	return nil
}

// passwordForms are the two ways a PKCS#12 password becomes bytes. RFC 7292 spells
// it as a BMPString (UTF-16BE with a terminating zero), which is what the legacy
// PBE schemes and Go's pkcs12 use; PBES2 with PBKDF2 comes from PKCS#5 v2.0, where
// the password is an octet string and JDK feeds it as UTF-8. Files exist in both
// shapes — keytool writes the second — so both are tried, and which one opened the
// bag is reported instead of guessed.
func passwordForms(password string) ([][]byte, error) {
	bmp, err := bmpString(password)
	if err != nil {
		return nil, err
	}
	return [][]byte{[]byte(password), bmp}, nil
}

// decryptPBE opens a PBES2 encrypted blob: PBKDF2 for the key, AES-CBC for the
// data, both with the parameters written in the file.
func decryptPBE(algo algoID, ciphertext []byte, password string) ([]byte, error) {
	if !algo.OID.Equal(oidPBES2) {
		return nil, fmt.Errorf("keystore encrypted with %s: apkc's Go signer reads PBES2 (PBKDF2 + AES-CBC) only; re-create it with keytool -storetype PKCS12 or build with --signer=jvm", algo.OID)
	}

	var params pbes2Params
	if _, err := asn1.Unmarshal(algo.Para.FullBytes, &params); err != nil {
		return nil, fmt.Errorf("PBES2 parameters: %v", err)
	}
	if !params.KDF.OID.Equal(oidPBKDF2) {
		return nil, fmt.Errorf("key derivation %s is not PBKDF2", params.KDF.OID)
	}

	keyBits, err := aesKeySize(params.Cipher.OID)
	if err != nil {
		return nil, err
	}
	length := keyBits / 8
	if params.KDF.Params.KeyLength > 0 {
		length = params.KDF.Params.KeyLength
	}
	prf, err := pbkdf2Hash(params.KDF.Params.PRF.OID)
	if err != nil {
		return nil, err
	}
	forms, err := passwordForms(password)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) == 0 || len(ciphertext)%(aes.BlockSize) != 0 {
		return nil, fmt.Errorf("ciphertext of %d bytes is not a whole number of AES blocks", len(ciphertext))
	}

	// each password form in turn: a wrong storepass is not mistaken for a right
	// one by luck of the padding, because what comes out is parsed as PKCS#8 or as
	// a certificate right after, and that is the real test of the key
	problems := []string{}
	for i, form := range forms {
		dk, err := pbkdf2.Key(prf, string(form), params.KDF.Params.Salt, params.KDF.Params.Iterations, length)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(dk)
		if err != nil {
			return nil, err
		}
		iv := params.Cipher.IVParams
		if len(iv) != block.BlockSize() {
			return nil, fmt.Errorf("cipher IV of %d bytes, expected %d", len(iv), block.BlockSize())
		}

		plain := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)
		out, err := unpadCBC(plain, block.BlockSize())
		if err == nil {
			if i > 0 {
				LogD("sign", "the keystore password is the PKCS#12 BMP form, not the PKCS#5 one")
			}
			return out, nil
		}
		problems = append(problems, err.Error())
	}
	return nil, fmt.Errorf("the keystore did not open with the given storepass: %s", strings.Join(problems, "; "))
}

func unpadCBC(plain []byte, size int) ([]byte, error) {
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > size || pad > len(plain) {
		return nil, fmt.Errorf("implausible CBC padding of %d bytes (wrong storepass?)", pad)
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("CBC padding is inconsistent (wrong storepass?)")
		}
	}
	return plain[:len(plain)-pad], nil
}

func aesKeySize(oid asn1.ObjectIdentifier) (int, error) {
	switch {
	case oid.Equal(oidAES128CBC):
		return 128, nil
	case oid.Equal(oidAES192CBC):
		return 192, nil
	case oid.Equal(oidAES256CBC):
		return 256, nil
	}
	return 0, fmt.Errorf("cipher %s is not AES-CBC", oid)
}

func pbkdf2Hash(oid asn1.ObjectIdentifier) (func() hash.Hash, error) {
	switch {
	case oid.Equal(oidHMACSHA1):
		return crypto.SHA1.New, nil
	case oid.Equal(oidHMACSHA256):
		return crypto.SHA256.New, nil
	case oid.Equal(oidHMACSHA512):
		return crypto.SHA512.New, nil
	}
	return nil, fmt.Errorf("PRF %s is not a known HMAC", oid)
}

// bmpString is how PKCS#12 feeds a password to PBKDF2: UTF-16BE with a
// terminating zero, without the byte that counts it.
func bmpString(s string) ([]byte, error) {
	out := make([]byte, 0, 2*len(s)+2)
	for _, r := range s {
		if r > 0xFFFF {
			return nil, fmt.Errorf("password character %q is outside BMP", r)
		}
		out = append(out, byte(r>>8), byte(r))
	}
	return append(out, 0, 0), nil
}

// octetStringOf unwraps the OCTET STRING that ContentInfo and the explicit
// wrappers carry their payload in.
func octetStringOf(b []byte) ([]byte, error) {
	var raw asn1.RawValue
	if rest, err := asn1.Unmarshal(b, &raw); err != nil {
		// some writers put the payload straight after the explicit tag
		if len(b) > 0 && b[0] == 0x30 {
			return b, nil
		}
		return nil, fmt.Errorf("expected an OCTET STRING: %v", err)
	} else if len(rest) != 0 {
		return nil, errors.New("trailing bytes after an OCTET STRING")
	}
	if raw.Class != 0 || raw.Tag != 4 {
		// an inner SEQUENCE rather than a string: the payload is the bytes as read
		if raw.Class == 0 && raw.Tag == 16 {
			return b, nil
		}
		return nil, fmt.Errorf("expected an OCTET STRING, found class %d tag %d", raw.Class, raw.Tag)
	}
	return raw.Bytes, nil
}
