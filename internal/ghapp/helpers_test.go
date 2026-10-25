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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "ghs_TESTTOKEN_abcdef123456"
	testOrg   = "acme"
)

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkcs1PEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// verifyJWT checks the RS256 signature and returns header and claims.
func verifyJWT(t *testing.T, jwt string, pub *rsa.PublicKey) (hdr, claims map[string]any) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	dec := func(s string) []byte {
		if strings.ContainsAny(s, "=+/") {
			t.Fatalf("segment is not base64 RawURL: %q", s)
		}
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if err := json.Unmarshal(dec(parts[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(dec(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], dec(parts[2])); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	return hdr, claims
}

type fakeGH struct {
	*httptest.Server
	mu         sync.Mutex
	pub        *rsa.PublicKey
	instID     int64
	expires    func() time.Time
	lookups    int
	mints      int
	lastBody   map[string]any
	lastJWT    string
	instStatus int
}

func newFakeGH(t *testing.T, pub *rsa.PublicKey) *fakeGH {
	f := &fakeGH{pub: pub, instID: 42, expires: func() time.Time { return time.Now().Add(time.Hour) }}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "no auth", 401)
			return
		}
		f.lastJWT = strings.TrimPrefix(auth, "Bearer ")
		switch {
		case r.Method == "GET" && r.URL.Path == "/orgs/"+testOrg+"/installation":
			f.lookups++
			if f.instStatus != 0 {
				http.Error(w, "echo "+f.lastJWT, f.instStatus)
				return
			}
			fmt.Fprintf(w, `{"id":%d}`, f.instID)
		case r.Method == "POST" && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", f.instID):
			f.mints++
			f.lastBody = nil
			b, _ := io.ReadAll(r.Body)
			if len(b) > 0 {
				if err := json.Unmarshal(b, &f.lastBody); err != nil {
					http.Error(w, "bad json", 400)
					return
				}
			}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, fmt.Sprintf("%s_%d", testToken, f.mints), f.expires().UTC().Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func verifies(jwt string, pub *rsa.PublicKey) bool {
	parts := strings.Split(jwt, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
}

func writeFile(p string, b []byte) error { return os.WriteFile(p, b, 0o600) }
