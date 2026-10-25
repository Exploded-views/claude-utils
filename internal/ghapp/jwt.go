// Package ghapp mints GitHub App installation tokens using only the standard
// library: an RS256 JWT identifies the app, and is exchanged for a short-lived
// installation access token.
//
// Secrets (key, JWT, token) are never logged, persisted, or included in errors.
package ghapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"time"
)

const (
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute // GitHub allows at most 10
)

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ParseKey parses an RSA private key in PEM form (PKCS#1 "RSA PRIVATE KEY" or
// PKCS#8 "PRIVATE KEY"). As a best effort it overwrites the pemBytes it is
// given and its own decoded DER copy with zeros before returning, so the
// caller must not reuse pemBytes. That is all it clears: the returned key's
// numbers, copies the caller or the runtime made earlier (a string, an I/O
// buffer, a garbage-collected heap) and swap are out of its reach.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	zero(pemBytes)
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}
	defer zero(block.Bytes)
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("private key is not RSA")
	}
	return nil, errors.New("private key is not a valid PKCS#1 or PKCS#8 RSA key")
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signJWT builds the app JWT: iat = now-60s (clock skew), exp = now+9m, iss =
// the app id as a string.
func signJWT(key *rsa.PrivateKey, appID string, now time.Time) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": appID,
	})
	signing := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("cannot sign the app JWT")
	}
	return signing + "." + b64(sig), nil
}
