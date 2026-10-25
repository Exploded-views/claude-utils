package ghapp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestJWTHeaderClaimsSignature(t *testing.T) {
	k := genKey(t)
	now := time.Unix(1_700_000_000, 0)
	jwt, err := signJWT(k, "12345", now)
	if err != nil {
		t.Fatal(err)
	}
	hdr, claims := verifyJWT(t, jwt, &k.PublicKey)
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
		t.Errorf("header = %v", hdr)
	}
	if claims["iss"] != "12345" {
		t.Errorf("iss = %#v, want string 12345", claims["iss"])
	}
	if int64(claims["iat"].(float64)) != now.Unix()-60 {
		t.Errorf("iat = %v, want now-60s", claims["iat"])
	}
	if int64(claims["exp"].(float64)) != now.Unix()+9*60 {
		t.Errorf("exp = %v, want now+9m", claims["exp"])
	}
}

func TestJWTSignatureRejectsWrongKey(t *testing.T) {
	k, other := genKey(t), genKey(t)
	jwt, _ := signJWT(k, "1", time.Now())
	if verifies(jwt, &other.PublicKey) {
		t.Fatal("signature verified under the wrong key")
	}
	if !verifies(jwt, &k.PublicKey) {
		t.Fatal("signature failed under the right key")
	}
}

func TestParseKeyPKCS1AndPKCS8(t *testing.T) {
	k := genKey(t)
	for name, pemBytes := range map[string][]byte{"pkcs1": pkcs1PEM(k), "pkcs8": pkcs8PEM(t, k)} {
		got, err := ParseKey(pemBytes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Equal(k) {
			t.Errorf("%s: parsed key differs", name)
		}
	}
	for _, bad := range [][]byte{nil, []byte("not pem"), []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestParseKeyZeroesInput(t *testing.T) {
	k := genKey(t)
	in := pkcs1PEM(k)
	if _, err := ParseKey(in); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, make([]byte, len(in))) {
		t.Error("key bytes not zeroed after parsing")
	}
}

func TestMintLooksUpInstallationAndPostsToken(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	tok, err := Mint(context.Background(), "777", pkcs8PEM(t, k), testOrg, gh.URL)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != testToken+"_1" || tok.ExpiresAt.Before(time.Now().Add(30*time.Minute)) {
		t.Errorf("token = %+v", tok)
	}
	if gh.lookups != 1 || gh.mints != 1 {
		t.Errorf("lookups=%d mints=%d", gh.lookups, gh.mints)
	}
	_, claims := verifyJWT(t, gh.lastJWT, &k.PublicKey)
	if claims["iss"] != "777" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if len(gh.lastBody) != 0 {
		t.Errorf("unscoped mint sent body %v", gh.lastBody)
	}
}

func TestMintNarrowedPermissionsAndRepos(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	_, err := MintWith(context.Background(), Params{
		AppID: "1", Key: pkcs1PEM(k), Org: testOrg, APIBase: gh.URL,
		Repositories: []string{"mem-personal"},
		Permissions:  map[string]string{"metadata": "read", "administration": "write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := gh.lastBody["repositories"].([]any)
	perms, _ := gh.lastBody["permissions"].(map[string]any)
	if len(repos) != 1 || repos[0] != "mem-personal" {
		t.Errorf("repositories = %v", gh.lastBody["repositories"])
	}
	if len(perms) != 2 || perms["metadata"] != "read" || perms["administration"] != "write" {
		t.Errorf("permissions = %v", perms)
	}
}

func TestMintRejectsBadOrg(t *testing.T) {
	k := genKey(t)
	for _, org := range []string{"", "a/b", "../x", "a b", "-x"} {
		if _, err := Mint(context.Background(), "1", pkcs1PEM(k), org, "http://127.0.0.1:1"); err == nil {
			t.Errorf("org %q accepted", org)
		}
	}
}

func TestMinterCachesAndRefreshes(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	clock := time.Now()
	exp := clock.Add(time.Hour)
	gh.expires = func() time.Time { return exp }
	loads := 0
	m := &Minter{AppID: "1", Org: testOrg, APIBase: gh.URL, Now: func() time.Time { return clock },
		LoadKey: func(context.Context) ([]byte, error) { loads++; return pkcs1PEM(k), nil }}
	ctx := context.Background()
	a, err := m.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(54 * time.Minute) // still > 5 min left
	b, _ := m.Token(ctx)
	if a != b || gh.mints != 1 || loads != 1 {
		t.Fatalf("expected cache hit: mints=%d loads=%d", gh.mints, loads)
	}
	clock = clock.Add(2 * time.Minute) // 4 min left -> refresh
	exp = clock.Add(time.Hour)
	c, err := m.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c == a || gh.mints != 2 {
		t.Errorf("expected refresh: mints=%d", gh.mints)
	}
}

func TestMintClockSkewIatBackdated(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	fixed := time.Unix(1_800_000_000, 0)
	_, err := MintWith(context.Background(), Params{AppID: "1", Key: pkcs1PEM(k), Org: testOrg, APIBase: gh.URL, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	_, claims := verifyJWT(t, gh.lastJWT, &k.PublicKey)
	if int64(claims["iat"].(float64)) != fixed.Unix()-60 || int64(claims["exp"].(float64)) != fixed.Unix()+540 {
		t.Errorf("claims = %v", claims)
	}
}

func TestSecretsNeverInErrors(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	gh.instStatus = 404 // the fake echoes the JWT in the error body
	pemBytes := pkcs1PEM(k)
	pemCopy := string(pemBytes)
	_, err := Mint(context.Background(), "1", pemBytes, testOrg, gh.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if gh.lastJWT == "" || strings.Contains(msg, gh.lastJWT) || strings.Contains(msg, strings.Split(gh.lastJWT, ".")[2]) {
		t.Errorf("error leaks JWT: %q", msg)
	}
	if strings.Contains(msg, "PRIVATE KEY") || strings.Contains(msg, pemCopy[40:80]) {
		t.Errorf("error leaks key: %q", msg)
	}
	// unreachable server: transport error must not contain secrets either
	gh.Close()
	_, err = Mint(context.Background(), "1", pkcs1PEM(k), testOrg, gh.URL)
	if err == nil || strings.Contains(err.Error(), "BEGIN") {
		t.Errorf("transport error = %v", err)
	}
}

func TestScrubRemovesSecrets(t *testing.T) {
	got := scrubText("boom abc.def.ghi and ghs_x here", "abc.def.ghi", "ghs_x")
	if strings.Contains(got, "abc.def.ghi") || strings.Contains(got, "ghs_x") {
		t.Errorf("scrub left secrets: %q", got)
	}
}

func TestMintCapsExpiryAtOneHour(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	clock := time.Now()
	gh.expires = func() time.Time { return clock.Add(240 * time.Hour) } // a lying server
	tok, err := MintWith(context.Background(), Params{AppID: "1", Key: pkcs1PEM(k), Org: testOrg, APIBase: gh.URL, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if !tok.ExpiresAt.Equal(clock.Add(time.Hour)) {
		t.Errorf("expiry = %v, want now+1h", tok.ExpiresAt.Sub(clock))
	}
	// the cache honours the cap: 61 minutes later it must mint again
	m := &Minter{AppID: "1", Org: testOrg, APIBase: gh.URL, Now: func() time.Time { return clock },
		LoadKey: func(context.Context) ([]byte, error) { return pkcs1PEM(k), nil }}
	m.Token(context.Background())
	clock = clock.Add(61 * time.Minute)
	gh.expires = func() time.Time { return clock.Add(time.Hour) }
	m.Token(context.Background())
	if gh.mints != 3 { // direct mint + two through the minter
		t.Errorf("mints = %d: a token with a bogus expiry stayed cached", gh.mints)
	}
}
