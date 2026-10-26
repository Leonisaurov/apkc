package main

import (
	"bytes"
	"crypto"
	"crypto/sha1"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// A JKS keystore is not an ASN.1 document and its keys are not encrypted with a
// standard scheme. The format comes from sun.security.provider.JavaKeyStore, and the
// protection of a private key from sun.security.provider.KeyProtector, which is neither
// PKCS#5 nor PKCS#12: it runs the password and the salt through SHA-1 round after round
// to make a keystream, XORs that over the key bytes, and appends SHA-1(password + key) so
// a wrong password is noticed instead of answered with garbage. Both are read here
// exactly as the JDK writes them, because a signing tool that cannot say "that is not the
// password" is worse than one that refuses.
//
// The file as a whole carries the same kind of checksum at the end, keyed with the store
// password and the string below, and it is verified on the way out.
const (
	jksPrivateEntry    = 1
	jksTrustedEntry    = 2
	jksSaltLen         = 20
	jksDigestLen       = 20
	jksMightyAphrodite = "Mighty Aphrodite"
)

// jksReader walks the big-endian records the format is made of. Every read is bounds
// checked: a keystore that stops in the middle is a file somebody truncated, and the
// answer has to be one line of error rather than a panic in the signing stage.
type jksReader struct {
	b   []byte
	at  int
	ver int
}

func (r *jksReader) u16() (uint16, error) {
	if r.at+2 > len(r.b) {
		return 0, errors.New("the keystore ends inside a 16 bit length")
	}
	v := binary.BigEndian.Uint16(r.b[r.at:])
	r.at += 2
	return v, nil
}

func (r *jksReader) u32() (uint32, error) {
	if r.at+4 > len(r.b) {
		return 0, errors.New("the keystore ends inside a 32 bit length")
	}
	v := binary.BigEndian.Uint32(r.b[r.at:])
	r.at += 4
	return v, nil
}

func (r *jksReader) take(n uint32) ([]byte, error) {
	if n > uint32(len(r.b)-r.at) {
		return nil, fmt.Errorf("the keystore claims %d bytes and has %d left", n, len(r.b)-r.at)
	}
	out := r.b[r.at : r.at+int(n)]
	r.at += int(n)
	return out, nil
}

// utf reads the modified UTF-8 string DataInputStream.writeUTF writes: two bytes of
// length and no terminator.
func (r *jksReader) utf() (string, error) {
	n, err := r.u16()
	if err != nil {
		return "", err
	}
	b, err := r.take(uint32(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// date skips the entry's creation timestamp, which is a millis field the signature has
// no use for; parsing it would only make a keystore with a clock skew look broken.
func (r *jksReader) date() error {
	_, err := r.take(8)
	return err
}

// jksPasswordBytes is the JDK's char[] to byte[]: every character as two bytes, big
// endian. Passing the password as ASCII here would verify a store digest for a different
// password than the one that made it.
func jksPasswordBytes(password string) []byte {
	u := utf16.Encode([]rune(password))
	out := make([]byte, 0, 2*len(u))
	for _, c := range u {
		out = append(out, byte(c>>8), byte(c))
	}
	return out
}

// unsealKey undoes what KeyProtector did. The bytes handed over are the JCE sealed
// object: a DER wrapper naming the protection algorithm, and inside it the salt, the
// XORed key and the digest that says whether the password was the right one.
func unsealKey(sealed, passwordBytes []byte) ([]byte, error) {
	var envelope struct {
		Algo asn1.RawValue
		Data []byte
	}
	if _, err := asn1.Unmarshal(sealed, &envelope); err != nil {
		return nil, fmt.Errorf("the protected key is not a JCE sealed object: %v", err)
	}
	if len(envelope.Data) < jksSaltLen+jksDigestLen {
		return nil, errors.New("the protected key is shorter than its salt and its checksum")
	}
	body := envelope.Data[jksSaltLen : len(envelope.Data)-jksDigestLen]
	tail := envelope.Data[len(envelope.Data)-jksDigestLen:]

	key := make([]byte, len(body))
	previous := envelope.Data[:jksSaltLen]
	for off := 0; off < len(body); {
		h := sha1.New()
		h.Write(passwordBytes)
		h.Write(previous)
		previous = h.Sum(nil)
		off += copy(key[off:], previous)
	}
	for i := range body {
		key[i] ^= body[i]
	}

	check := sha1.New()
	check.Write(passwordBytes)
	check.Write(key)
	if !bytes.Equal(check.Sum(nil), tail) {
		return nil, errors.New("the password does not open this JKS keystore, or the file has been altered")
	}
	return key, nil
}

// readJKS opens a JKS blob with the store password and returns the entry named alias,
// falling back to the first key the file holds; which of those two happened is what the
// caller has to know, so the answer carries the name it used and how many keys there are
// in the file, exactly as the PKCS#12 reader does.
func readJKS(blob []byte, password, alias string) (keyMaterial, error) {
	if len(blob) < 12 {
		return keyMaterial{}, errors.New("the keystore is too short to be a JKS file")
	}
	r := &jksReader{b: blob}
	magic, err := r.u32()
	if err != nil {
		return keyMaterial{}, err
	}
	if magic != jksMagic {
		return keyMaterial{}, fmt.Errorf("not a JKS keystore: magic %08x", magic)
	}
	version, err := r.u32()
	if err != nil {
		return keyMaterial{}, err
	}
	if version != 1 && version != 2 {
		return keyMaterial{}, fmt.Errorf("JKS version %d is not one this reader knows", version)
	}
	r.ver = int(version)
	count, err := r.u32()
	if err != nil {
		return keyMaterial{}, err
	}

	material := keyMaterial{MacAlgo: "SHA-1 store digest"}
	type jksKey struct {
		alias  string
		signer crypto.Signer
		cert   *x509.Certificate
	}
	keys := []jksKey{}
	for i := uint32(0); i < count; i++ {
		tag, err := r.u32()
		if err != nil {
			return keyMaterial{}, err
		}
		name, err := r.utf()
		if err != nil {
			return keyMaterial{}, err
		}
		if err := r.date(); err != nil {
			return keyMaterial{}, err
		}
		switch tag {
		case jksPrivateEntry:
			sealedLen, err := r.u32()
			if err != nil {
				return keyMaterial{}, err
			}
			sealed, err := r.take(sealedLen)
			if err != nil {
				return keyMaterial{}, err
			}
			certs, err := r.certs()
			if err != nil {
				return keyMaterial{}, err
			}
			pb := jksPasswordBytes(password)
			der, err := unsealKey(sealed, pb)
			if err != nil {
				return keyMaterial{}, err
			}
			signer, err := parsePrivateKey(der)
			if err != nil {
				return keyMaterial{}, err
			}
			cert, err := chainCert(signer, certs)
			if err != nil {
				return keyMaterial{}, fmt.Errorf("entry %s: %v", name, err)
			}
			keys = append(keys, jksKey{alias: name, signer: signer, cert: cert})
		case jksTrustedEntry:
			// a trusted certificate entry cannot sign anything: it holds no key. It is
			// read past, not refused, because a keystore of certs plus one key is a
			// normal thing to find and the key is what was asked for.
			if r.ver == 2 {
				if _, err := r.utf(); err != nil {
					return keyMaterial{}, err
				}
			}
			derLen, err := r.u32()
			if err != nil {
				return keyMaterial{}, err
			}
			if _, err := r.take(derLen); err != nil {
				return keyMaterial{}, err
			}
		default:
			return keyMaterial{}, fmt.Errorf("unknown JKS entry type %d at entry %d", tag, i)
		}
	}

	// what is left is the checksum of everything read, keyed with the password
	rest, err := r.take(jksDigestLen)
	if err != nil {
		return keyMaterial{}, err
	}
	pb := jksPasswordBytes(password)
	h := sha1.New()
	h.Write(pb)
	h.Write([]byte(jksMightyAphrodite))
	h.Write(blob[:r.at-jksDigestLen])
	if !bytes.Equal(h.Sum(nil), rest) {
		return keyMaterial{}, errors.New("this JKS keystore was tampered with, or the password is wrong")
	}

	if len(keys) == 0 {
		return keyMaterial{}, errors.New("the JKS keystore holds no private key apkc can sign with")
	}
	material.Keys = len(keys)
	material.Names = true // a JKS entry is always named: the alias is in the file
	chosen := keys[0]
	for _, k := range keys {
		if strings.EqualFold(k.alias, alias) {
			chosen = k
			break
		}
	}
	material.Signer, material.Cert, material.Alias = chosen.signer, chosen.cert, chosen.alias
	return material, nil
}

// certs reads the certificate chain of a private key entry: version 2 names the type of
// each certificate before its bytes, version 1 does not, and only X.509 can sign anything.
func (r *jksReader) certs() ([]*x509.Certificate, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	out := []*x509.Certificate{}
	for i := uint32(0); i < n; i++ {
		if r.ver == 2 {
			kind, err := r.utf()
			if err != nil {
				return nil, err
			}
			if kind != "X.509" {
				return nil, fmt.Errorf("a %s certificate is not something a package is signed with", kind)
			}
		}
		derLen, err := r.u32()
		if err != nil {
			return nil, err
		}
		der, err := r.take(derLen)
		if err != nil {
			return nil, err
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("the certificate of the keystore entry cannot be read: %v", err)
		}
		out = append(out, cert)
	}
	return out, nil
}

// chainCert takes the certificate that belongs to the recovered key rather than the one
// that happens to be first in the chain: the order is written by whatever made the
// keystore, and signing with one identity while shipping another's certificate installs
// once and then fails every update against the old signature.
func chainCert(signer crypto.Signer, certs []*x509.Certificate) (*x509.Certificate, error) {
	if len(certs) == 0 {
		return nil, errors.New("the key entry has no certificate to travel with it")
	}
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, err
	}
	for _, c := range certs {
		if bytes.Equal(c.RawSubjectPublicKeyInfo, public) {
			return c, nil
		}
	}
	return nil, errors.New("none of the certificates in the entry belongs to its key")
}
