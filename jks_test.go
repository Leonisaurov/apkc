package main

import (
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"
)

// Las tres tiendas estan aqui en claro, y no en ficheros aparte, porque el test tiene
// que correr en cualquier maquina sin un jdk delante con lo que keytool escribio de
// verdad: son dos codificaciones de UNA clave (JKS y PKCS#12) y una tienda sin clave.
// una tienda JKS escrita por keytool (JDK 21) con alias androiddebugkey, RSA 2048 y contrasena android.
const jksFixture = "" +
	"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" +
	""

// la MISMA tienda convertida a PKCS#12 por keytool -importkeystore: mismo par clave-certificado, otro envase.
const p12Fixture = "" +
	"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" +
	""

// una tienda JKS sin clave privada: un solo trustedCertEntry, que no firma nada.
const trustFixture = "" +
	"/u3+7QAAAAIAAAABAAAAAgAGbXljZXJ0AAABoXa3Wn8ABVguNTA5AAADBTCCAwEwggHp" +
	"oAMCAQICCH4Z9yfdDC0JMA0GCSqGSIb3DQEBDAUAMC8xCzAJBgNVBAYTAlVTMQ0wCwYD" +
	"VQQKEwRUZXN0MREwDwYDVQQDEwhUZXN0IEpLUzAeFw0yNjEwMjYwNTIzNDRaFw0yNjEx" +
	"MjUwNTIzNDRaMC8xCzAJBgNVBAYTAlVTMQ0wCwYDVQQKEwRUZXN0MREwDwYDVQQDEwhU" +
	"ZXN0IEpLUzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBALyIpSuXpGrq1D7v" +
	"ztYf4QgLTHfviuTpP0zXY6MY+B6hzRrs9KalPQKFsP17oEjLZDfFZ16Hkq7bZAiwEs7g" +
	"AR1xINXFBJ56lu9jNV4aYfib+notf4z6vN50W4luwNl27MXUltu4ct4JsEtT6Fnh1X8X" +
	"XFQxzQs1PwaDQpDcLWN2im+KTHCnBRUJK2ucDSCfrSvZ8U9xe+0y+v5Boqw2lz8iKt7S" +
	"T3S9KxeUGR0Fv2cCA/0nNFQ8+8j/BffuW0fHnrW88iIdBBu59no8xjVhWvLMvakRwggG" +
	"DO9q2XflfnAMsyWcCxEDxytrT17+I0OuEXJg3gDxG20OLfiPirL+JfMCAwEAAaMhMB8w" +
	"HQYDVR0OBBYEFNBJ5jyvU41UjkYl5owrU9sc54drMA0GCSqGSIb3DQEBDAUAA4IBAQC5" +
	"r+mN2qTYvl2kozP8lqezYyvqionbPCRUPZgBu/QlrZRhjg2NszKmYWmlZ3o31z6fXUx5" +
	"oMLsiPKs+9pijId31rpB87CeyNEhuT8WQ0YuYBFpEr1YpaOQEAN/HH8dVl3zXLLX5wG2" +
	"2ZPiEBuTlzh6TcGqUTW9H6ABPvFYcnYIG4PxtwOHksuDq9YG1U35Kpcv6bxmCcwjTWXt" +
	"tLgOG6bOB078620EQH0DsCQB3DSjWsA5dXHVljPfl4TaRPD3a4NPHljSjwp2zJ/9QI3N" +
	"sioXCWhmMSQPV1y3o/0tXkIJTZhKxbE7SSbPrgKLjFplpjC924F6RkqGWSFJjco5oaRA" +
	"R3n70kweP27p0XXUdgPDM5ESOis=" +
	""

// TestJKSAndPKCS12AreTheSameKey is the reason a JKS reader is trustworthy without a
// device running API 16: keytool wrote the store twice, once in each format, and the two
// readers in this package have to hand the signing stage the same key and the same
// certificate. Anything else and the format a project was stored in would decide the
// identity its package is signed with.
func TestJKSAndPKCS12AreTheSameKey(t *testing.T) {
	jksBlob, err := base64.StdEncoding.DecodeString(jksFixture)
	if err != nil {
		t.Fatal(err)
	}
	p12Blob, err := base64.StdEncoding.DecodeString(p12Fixture)
	if err != nil {
		t.Fatal(err)
	}

	fromJKS, err := readJKS(jksBlob, "android", "androiddebugkey")
	if err != nil {
		t.Fatalf("the store keytool wrote in 2026 does not open: %v", err)
	}
	fromP12, err := readPKCS12(p12Blob, "android", "androiddebugkey")
	if err != nil {
		t.Fatalf("the same store as a PKCS#12 does not open: %v", err)
	}

	a, err := x509.MarshalPKCS8PrivateKey(fromJKS.Signer)
	if err != nil {
		t.Fatal(err)
	}
	b, err := x509.MarshalPKCS8PrivateKey(fromP12.Signer)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("the two formats answered with different keys: an apk signed by one would not update the other")
	}
	if !fromJKS.Cert.Equal(fromP12.Cert) {
		t.Error("the two formats answered with different certificates")
	}
	if fromJKS.Alias != "androiddebugkey" {
		t.Errorf("the alias has to come from the file, it is %q", fromJKS.Alias)
	}
	if fromJKS.Keys != 1 || !fromJKS.Names {
		t.Errorf("one named key expected, got %d keys, names %v", fromJKS.Keys, fromJKS.Names)
	}
}

// TestJKSRefusesAWrongPassword antes de devolver basura: el sello del KeyProtector se
// comprueba, y ahi es donde un lector que no lo hace firma con bytes sin sentido.
func TestJKSRefusesAWrongPassword(t *testing.T) {
	blob, err := base64.StdEncoding.DecodeString(jksFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readJKS(blob, "not-the-password", "androiddebugkey"); err == nil {
		t.Fatal("a wrong password opened a keystore: the signing key is not what the caller asked for")
	} else if !strings.Contains(err.Error(), "password") {
		t.Errorf("the refusal has to name the password as the suspect: %v", err)
	}
}

// TestJKSRefusesATamperedFile: el checksum final es lo unico que distingue un keystore
// editado a mano de uno intacto, y un lector que no lo mira firma igual.
func TestJKSRefusesATamperedFile(t *testing.T) {
	blob, err := base64.StdEncoding.DecodeString(jksFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readJKS(blob, "android", "androiddebugkey"); err != nil {
		t.Fatalf("the fixture does not open at all: %v", err)
	}
	cut := append([]byte{}, blob...)
	cut[len(cut)-1] ^= 0xff
	if _, err := readJKS(cut, "android", "androiddebugkey"); err == nil {
		t.Fatal("the store checksum was altered and the file still opened")
	}

	// y cada truncamiento tiene que ser un error, no un panic
	for n := 1; n < len(blob); n += 37 {
		if _, err := readJKS(blob[:n], "android", "androiddebugkey"); err == nil {
			t.Errorf("a keystore cut at %d bytes answered without an error", n)
		}
	}

	// una cabecera que no es JKS se dice por el numero magico
	if _, err := readJKS(append([]byte{0x00, 0x11, 0x22, 0x33}, blob[4:]...), "android", "androiddebugkey"); err == nil {
		t.Error("a file that is not a JKS store was accepted")
	}
}

// TestJKSWithoutAKeyIsRefused: un almacen de certificados de confianza no firma nada, y
// dejar pasar la ronda para fallar al firmar seria decirlo en el momento peor.
func TestJKSWithoutAKeyIsRefused(t *testing.T) {
	blob, err := base64.StdEncoding.DecodeString(trustFixture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readJKS(blob, "android", "mycert")
	if err == nil {
		t.Fatal("a keystore holding only a trusted certificate was accepted for signing")
	}
	if !strings.Contains(err.Error(), "no private key") {
		t.Errorf("the refusal has to say there is no key: %v", err)
	}
	if !keystoreHeadIsReadable(blob[:4]) {
		t.Error("a JKS store has to be reported as readable, even when it holds nothing usable")
	}
}
