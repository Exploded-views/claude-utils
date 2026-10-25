package ghapp

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"regexp"
)

var fpRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}=$`)

// Fingerprint is the key fingerprint GitHub shows in an app's settings:
// "SHA256:" and the standard base64 of the SHA-256 of the DER-encoded public
// key (PKIX SubjectPublicKeyInfo). This is what GitHub's documented check
// computes:
//
//	openssl rsa -in key.pem -pubout -outform DER | openssl sha256 -binary | openssl base64
func Fingerprint(pub *rsa.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:])
}

// KeyFingerprint parses a PEM private key (zeroing pemBytes) and returns its
// fingerprint.
func KeyFingerprint(pemBytes []byte) (string, error) {
	k, err := ParseKey(pemBytes)
	if err != nil {
		return "", err
	}
	return Fingerprint(&k.PublicKey), nil
}

var errFingerprint = errors.New("the app key does not match the key fingerprint (keyFingerprint) in apps.json; refusing to use it")
