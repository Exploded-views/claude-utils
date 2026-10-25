package ghapp

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os/exec"
	"strings"
	"testing"
)

// Vector produced with GitHub's documented command
//
//	openssl rsa -in k.pem -pubout -outform DER | openssl sha256 -binary | openssl base64
//
// on a throwaway key (only the public half is kept here). GitHub's docs give
// no sample value, so the pipeline itself is the reference.
const vectorPub = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAtXac5+1ZZA21eOXEirxh
zQM9Q70bJfSXtFoEGYLG3zFDsUzZPXDynQDUOoGs/rIZLSmyjQQUvImkbxm6rtS9
01q1/3G6GvEhpDuvj6K9LC2sbUZxfaqALVQCny6635ehiu96kwO4mzsiIHW4v6L5
HOesQNDyGJ6MQO49EUQSYgzlcsQGjPG3vP9xvdhw4s+DAYsxfmziGiZAuA4UqSWY
BJ8Zy33WHAPuzwqz3Y6wxoCejqO4EkvUCuOepjZCc9W7+WSECRIJNy4KIFs4ok+1
HVVwWT6mnchRpbnwgzYDklwoZLj3OLNFfK0VlemHI3SHJzJIsEs05gfAwjrGkych
twIDAQAB
-----END PUBLIC KEY-----
`
const vectorFP = "SHA256:PsfSS3Z33ML/IRmJAfxkMGp0FqKAGMoYHouXrspjvWY="

func TestFingerprintKnownVector(t *testing.T) {
	blk, _ := pem.Decode([]byte(vectorPub))
	pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(pk.(*rsa.PublicKey)); got != vectorFP {
		t.Errorf("got %s want %s", got, vectorFP)
	}
}

func TestKeyFingerprintFromPrivatePEMMatchesOpenSSL(t *testing.T) {
	k := genKey(t)
	for name, p := range map[string][]byte{"pkcs1": pkcs1PEM(k), "pkcs8": pkcs8PEM(t, k)} {
		want := Fingerprint(&k.PublicKey)
		got, err := KeyFingerprint(p)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed; the known vector above still covers the format")
	}
	dir := t.TempDir()
	kp := dir + "/k.pem"
	if err := writeFile(kp, pkcs1PEM(k)); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", "openssl rsa -in "+kp+" -pubout -outform DER 2>/dev/null | openssl sha256 -binary | openssl base64").Output()
	if err != nil {
		t.Skip("openssl pipeline unavailable")
	}
	if want := "SHA256:" + strings.TrimSpace(string(out)); Fingerprint(&k.PublicKey) != want {
		t.Errorf("differs from openssl: %s vs %s", Fingerprint(&k.PublicKey), want)
	}
}

func TestKeyFingerprintBadInput(t *testing.T) {
	if _, err := KeyFingerprint([]byte("nope")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestMintRefusesKeyThatDoesNotMatchFingerprint(t *testing.T) {
	k, other := genKey(t), genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	_, err := MintWith(context.Background(), Params{AppID: "1", Key: pkcs1PEM(k), Org: testOrg, APIBase: gh.URL,
		Fingerprint: Fingerprint(&other.PublicKey)})
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("err = %v", err)
	}
	if gh.lookups != 0 {
		t.Error("contacted GitHub with a mismatched key")
	}
	if _, err := MintWith(context.Background(), Params{AppID: "1", Key: pkcs1PEM(k), Org: testOrg, APIBase: gh.URL,
		Fingerprint: Fingerprint(&k.PublicKey)}); err != nil {
		t.Errorf("matching fingerprint refused: %v", err)
	}
}

func TestAppValidateFingerprintFormat(t *testing.T) {
	a := App{Name: "a", AppID: "1", Org: "o", KeyFile: "/k", KeyFingerprint: "not-a-fingerprint"}
	if a.Validate() == nil {
		t.Error("bad fingerprint accepted")
	}
	a.KeyFingerprint = vectorFP
	if err := a.Validate(); err != nil {
		t.Error(err)
	}
}

func TestFingerprintAcceptedPaddedOrNot(t *testing.T) {
	for _, fp := range []string{vectorFP, strings.TrimSuffix(vectorFP, "=")} {
		a := App{Name: "a", AppID: "1", Org: "o", KeyFile: "/k", KeyFingerprint: fp}
		if err := a.Validate(); err != nil {
			t.Errorf("%q: %v", fp, err)
		}
	}
	if (App{Name: "a", AppID: "1", Org: "o", KeyFile: "/k", KeyFingerprint: vectorFP + "="}).Validate() == nil {
		t.Error("double padding accepted")
	}
	if !SameFingerprint(vectorFP, strings.TrimSuffix(vectorFP, "=")) || SameFingerprint(vectorFP, "SHA256:"+strings.Repeat("A", 43)) {
		t.Error("SameFingerprint")
	}
}

func TestMintFingerprintMatchesEitherForm(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	for _, fp := range []string{Fingerprint(&k.PublicKey), strings.TrimSuffix(Fingerprint(&k.PublicKey), "=")} {
		if _, err := MintWith(context.Background(), Params{AppID: "1", Key: pkcs1PEM(k), Org: testOrg, APIBase: gh.URL, Fingerprint: fp}); err != nil {
			t.Errorf("%q refused: %v", fp, err)
		}
	}
}
