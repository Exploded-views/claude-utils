package scrub

import (
	"strings"
	"testing"
)

// Fixtures are assembled at run time so this file never contains a literal
// that looks like a credential (secret scanners would flag the repo itself).
func rep(prefix string, n int, ch string) string { return prefix + strings.Repeat(ch, n) }

func TestScan_Rules(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // expected rule name
	}{
		{"rsa key", "notes\n-----BEGIN RSA PRIVATE KEY-----\nabc\n", "private-key"},
		{"openssh key", "-----BEGIN OPENSSH PRIVATE KEY-----\n", "private-key"},
		{"plain key", "-----BEGIN PRIVATE KEY-----\n", "private-key"},
		{"ghp", "token is " + rep("ghp_", 36, "a"), "github-token"},
		{"gho", rep("gho_", 36, "B"), "github-token"},
		{"ghu", rep("ghu_", 36, "1"), "github-token"},
		{"ghs", rep("ghs_", 36, "c"), "github-token"},
		{"ghr", rep("ghr_", 36, "d"), "github-token"},
		{"fine-grained pat", rep("github_pat_", 40, "e"), "github-token"},
		{"openai key", "key: " + rep("sk-", 32, "a"), "openai-api-key"},
		{"openai project key", rep("sk-proj-", 30, "a"), "openai-api-key"},
		{"anthropic key", rep("sk-ant-api03-", 30, "a"), "anthropic-api-key"},
		{"slack bot", rep("xoxb-", 20, "1"), "slack-token"},
		{"slack user", rep("xoxp-", 20, "1"), "slack-token"},
		{"slack app-level", rep("xoxa-", 20, "1"), "slack-token"},
		{"aws access key", "id " + rep("AKIA", 16, "A"), "aws-access-key"},
		{"jwt", rep("eyJ", 12, "a") + "." + rep("eyJ", 12, "b") + "." + rep("", 16, "c"), "jwt"},
		{"password assignment", `password = "` + strings.Repeat("x", 16) + `"`, "credential-assignment"},
		{"token colon", `"api_key": "` + strings.Repeat("y", 20) + `"`, "credential-assignment"},
		{"secret single quote", `client_secret='` + strings.Repeat("z", 24) + `'`, "credential-assignment"},
		{"api-key mixed case", `API_KEY = "` + strings.Repeat("q", 16) + `"`, "credential-assignment"},
	}
	s := New(nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.Scan([]byte(c.content))
			if len(got) == 0 {
				t.Fatalf("no rule matched %q", c.name)
			}
			found := false
			for _, g := range got {
				if g == c.want {
					found = true
				}
			}
			if !found {
				t.Errorf("got rules %v, want %q", got, c.want)
			}
		})
	}
}

func TestScan_CleanContent(t *testing.T) {
	clean := []string{
		"---\nname: build notes\ntype: feedback\n---\nRun go test ./... before pushing.\n",
		"Use the sk-learn pipeline docs for the model.\n",
		"risk-assessment-for-the-quarterly-review is done\n",
		"The password is stored in the vault, never in this file.\n",
		`password = "short"`,
		`token = "` + strings.Repeat("x", 15) + `"`, // one under the minimum
		"ghp_tooshort\n",
		"AKIA is the prefix AWS uses\n",
		"see https://example.com/eyJ/docs\n",
	}
	s := New(nil)
	for _, c := range clean {
		if got := s.Scan([]byte(c)); len(got) != 0 {
			t.Errorf("Scan(%q) = %v, want none", c, got)
		}
	}
}

func TestScan_ReturnsEachRuleOnce(t *testing.T) {
	content := rep("ghp_", 36, "a") + "\n" + rep("ghp_", 36, "b") + "\n"
	got := New(nil).Scan([]byte(content))
	if len(got) != 1 || got[0] != "github-token" {
		t.Errorf("got %v, want [github-token]", got)
	}
}

func TestParseDenylist(t *testing.T) {
	src := "# comment\n\n  \nacme-corp\\.internal\r\n^client: .*\n"
	d, err := ParseDenylist(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 {
		t.Fatalf("got %d patterns, want 2", len(d))
	}
}

func TestDenylist_CaseInsensitiveAndRuleName(t *testing.T) {
	d, err := ParseDenylist(strings.NewReader("# header\nproject-nightjar\nwidget\\d+\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(d)
	got := s.Scan([]byte("working on Project-NightJar today"))
	if len(got) != 1 || got[0] != "denylist:2" {
		t.Errorf("got %v, want [denylist:2] (line number of the pattern)", got)
	}
	if got := s.Scan([]byte("WIDGET42")); len(got) != 1 || got[0] != "denylist:3" {
		t.Errorf("got %v, want [denylist:3]", got)
	}
	if got := s.Scan([]byte("nothing to see")); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestParseDenylist_InvalidPattern(t *testing.T) {
	_, err := ParseDenylist(strings.NewReader("ok\n(unclosed\n"))
	if err == nil {
		t.Fatal("want error for invalid regex")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the line: %v", err)
	}
	// The error must not echo the pattern text back (it may be sensitive).
	if strings.Contains(err.Error(), "unclosed") {
		t.Errorf("error leaks pattern: %v", err)
	}
}

func TestScan_CredentialAssignmentTable(t *testing.T) {
	long := strings.Repeat("v", 24)
	cases := []struct {
		in    string
		match bool
	}{
		// Real assignments.
		{`"api_key": "` + long + `"`, true},
		{`client_secret: "` + long + `"`, true},
		{`client_secret: '` + long + `'`, true},
		{`DB_PASSWORD="` + long + `"`, true},
		{`"access_token": "` + long + `"`, true},
		{`secret_key = "` + long + `"`, true},
		{`apiKey: "` + long + `"`, true},
		{`password_value: "` + long + `"`, true},
		{`api-key_string = "` + long + `"`, true},
		// Words that merely start with a keyword.
		{`"tokenizer": "bert-base-uncased-large"`, false},
		{`secretName: "letsencrypt-prod-tls-cert"`, false},
		{`"token_type": "refresh_token_v2_long"`, false},
		{`tokens: "` + long + `"`, false},
		{`passwordless: "` + long + `"`, false},
		{`secretRef: "` + long + `"`, false},
		{`token_endpoint: "https://example.com/oauth2/token"`, false},
		// Still conservative about short or unquoted values.
		{`password: "` + strings.Repeat("v", 15) + `"`, false},
		{`password: ` + long, false},
	}
	s := New(nil)
	for _, c := range cases {
		got := false
		for _, r := range s.Scan([]byte(c.in)) {
			if r == "credential-assignment" {
				got = true
			}
		}
		if got != c.match {
			t.Errorf("Scan(%q) credential-assignment = %v, want %v", c.in, got, c.match)
		}
	}
}

// The new rules are built from pieces so no token-shaped literal is in the file.
func TestScan_MoreRules(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"url password", "dsn " + "postgres://" + "svc_user" + ":" + "s3cr3tPw9x" + "@db.example.com:5432/app", "url-credentials"},
		{"url password https", "https://" + "alice" + ":" + "hunter2hunter2" + "@example.com/x", "url-credentials"},
		{"openai svcacct", rep("sk-svcacct-", 30, "a"), "openai-api-key"},
		{"openai admin", rep("sk-admin-", 30, "a"), "openai-api-key"},
		{"stripe sk", rep("sk_live_", 24, "a"), "stripe-key"},
		{"stripe rk", rep("rk_live_", 24, "B"), "stripe-key"},
		{"google api key", rep("AIza", 35, "a"), "google-api-key"},
		{"gitlab pat", rep("glpat-", 20, "a"), "gitlab-token"},
		{"aws secret", "aws_secret_access_key = " + rep("", 40, "a"), "aws-secret-key"},
		{"aws secret upper", "AWS_SECRET_ACCESS_KEY=" + rep("", 40, "A"), "aws-secret-key"},
		{"github after underscore", "GITHUB_TOKEN_" + rep("ghp_", 36, "a"), "github-token"},
		{"stripe after underscore", "STRIPE_" + rep("sk_live_", 24, "a"), "stripe-key"},
		{"github after equals", "GH=" + rep("gho_", 36, "a"), "github-token"},
	}
	s := New(nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, g := range s.Scan([]byte(c.content)) {
				if g == c.want {
					return
				}
			}
			t.Errorf("rule %q did not match %q", c.want, c.name)
		})
	}
}

func TestScan_MoreRulesCleanContent(t *testing.T) {
	clean := []string{
		// Placeholders in URLs.
		"postgres://user:password@localhost:5432/db",
		"https://" + "alice:" + "<password>" + "@example.com",
		"https://" + "alice:" + "${DB_PASS}" + "@example.com",
		"https://" + "alice:" + "{{ .Pass }}" + "@example.com",
		"mysql://root:changeme@db",
		"https://user:pass@example.com",
		// No password, or not userinfo.
		"ssh://git@github.com/org/repo.git",
		"https://example.com:8080/path@x",
		"mailto:someone@example.com",
		// Near misses for the key formats.
		"sk-svcacct-short",
		"sk_live_ is the Stripe live prefix",
		"sk_test_" + strings.Repeat("a", 24),
		"AIza is a Google prefix; AIzaShort",
		"glpat-short",
		"aws_secret_access_key = <your secret key goes here, 40 chars>",
		"aws_secret_access_key = $" + strings.Repeat("A", 39),
		"aws_secret_access_key = " + strings.Repeat("a", 39),
	}
	s := New(nil)
	for _, c := range clean {
		if got := s.Scan([]byte(c)); len(got) != 0 {
			t.Errorf("Scan(%q) = %v, want none", c, got)
		}
	}
}

func TestParseDenylist_UTF8BOM(t *testing.T) {
	// Without stripping, the BOM would be part of line 1's pattern and the
	// pattern would never match at the start of a file's content.
	d, err := ParseDenylist(strings.NewReader("\xef\xbb\xbfproject-nightjar\nwidget\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 {
		t.Fatalf("got %d patterns, want 2", len(d))
	}
	if got := New(d).Scan([]byte("about project-nightjar")); len(got) != 1 || got[0] != "denylist:1" {
		t.Errorf("got %v, want [denylist:1]", got)
	}
	// A BOM before a comment must not turn the comment into a pattern.
	d, err = ParseDenylist(strings.NewReader("\xef\xbb\xbf# header\nwidget\n"))
	if err != nil || len(d) != 1 {
		t.Errorf("BOM + comment: %d patterns, err %v; want 1, nil", len(d), err)
	}
}

func TestParseDenylist_RejectsUTF16(t *testing.T) {
	utf16le := "\xff\xfe" + "w\x00i\x00d\x00g\x00e\x00t\x00\n\x00"
	utf16be := "\xfe\xff" + "\x00w\x00i\x00d\x00g\x00e\x00t\x00\n"
	noBOM := "w\x00i\x00d\x00g\x00e\x00t\x00\n\x00"
	for name, src := range map[string]string{"le bom": utf16le, "be bom": utf16be, "no bom, NULs": noBOM, "NUL late": "ok\nwid\x00get\n"} {
		_, err := ParseDenylist(strings.NewReader(src))
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Errorf("%s: error should say the file must be UTF-8: %v", name, err)
		}
	}
}

// Documented behavior: a pattern that can match the empty string matches
// everything, contents and paths alike.
func TestDenylist_EmptyMatchingPatternHoldsBackEverything(t *testing.T) {
	for _, pat := range []string{".*", "a*", "foo|", "^"} {
		d, err := ParseDenylist(strings.NewReader(pat + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		s := New(d)
		if got := s.Scan([]byte("harmless")); len(got) != 1 {
			t.Errorf("%q: content not held back: %v", pat, got)
		}
		if !s.MatchesPath("projects/p1/memory/x.md") {
			t.Errorf("%q: path not held back", pat)
		}
	}
}
