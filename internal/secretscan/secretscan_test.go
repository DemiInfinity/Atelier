package secretscan

import (
	"encoding/base64"
	"math/rand/v2"
	"strings"
	"testing"
)

// Fake secrets are built at runtime so this file never contains anything a
// scanner (ours, or a code host's push protection) would flag.

const (
	alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	hexch = "0123456789abcdef"
)

var rng = rand.New(rand.NewPCG(1, 2))

func rnd(n int, set string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = set[rng.IntN(len(set))]
	}
	return string(b)
}

func join(parts ...string) string { return strings.Join(parts, "") }

// Fake values for each rule.
func fakeGitHub() string    { return join("gh", "p_", rnd(36, alnum)) }
func fakeAtelier() string   { return join("at", "l_", rnd(40, hexch)) }
func fakeAnthropic() string { return join("sk-", "ant-", "api03-", rnd(93, alnum+"-_"), "AA") }
func fakeAWS() string       { return join("AK", "IA", rnd(16, upnum)) }
func fakeStripe() string    { return join("sk", "_live_", rnd(32, alnum)) }
func fakeGoogle() string    { return join("AI", "za", rnd(35, alnum)) }
func fakeSlack() string {
	return join("xo", "xb-", rnd(12, "0123456789"), "-", rnd(13, "0123456789"), "-", rnd(24, alnum))
}

func ids(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.RuleID)
	}
	return out
}

func TestRulesTruePositives(t *testing.T) {
	s := New(nil)
	cases := []struct {
		rule, line string
	}{
		{"github-pat", `token := "` + fakeGitHub() + `"`},
		{"github-fine-grained-pat", "GH=" + join("github", "_pat_", rnd(22, alnum), "_", rnd(59, alnum))},
		{"atelier-access-token", "password: " + fakeAtelier()},
		{"atelier-job-token", "echo " + join("at", "j_", rnd(64, hexch))},
		{"atelier-runner-token", join("at", "r_", rnd(64, hexch))},
		{"atelier-registration-token", "ATELIER_REG=" + join("at", "reg_", rnd(48, hexch))},
		{"anthropic-api-key", "ANTHROPIC_API_KEY=" + fakeAnthropic()},
		{"openai-api-key", "OPENAI_API_KEY=" + join("sk", "-proj-", rnd(60, alnum+"_-"))},
		{"aws-access-key-id", "aws_access_key_id = " + fakeAWS()},
		{"aws-secret-access-key", `aws_secret_access_key = "` + rnd(40, alnum+"/+") + `"`},
		{"google-api-key", `apiKey: "` + fakeGoogle() + `"`},
		{"slack-token", "SLACK=" + fakeSlack()},
		{"stripe-secret-key", "stripe.Key = \"" + fakeStripe() + "\""},
		{"generic-secret", `client_secret = "` + rnd(32, alnum) + `"`},
	}
	for _, c := range cases {
		fs := s.ScanText(c.line)
		found := false
		for _, f := range fs {
			if f.RuleID == c.rule {
				found = true
				if strings.Contains(f.Redacted, f.Secret()) || len(f.Redacted) > 8+len("…") {
					t.Errorf("%s: redacted %q reveals too much", c.rule, f.Redacted)
				}
				if f.Fingerprint != Fingerprint(f.Secret()) || len(f.Fingerprint) != 24 {
					t.Errorf("%s: fingerprint %q", c.rule, f.Fingerprint)
				}
			}
		}
		if !found {
			t.Errorf("%s not found in %q (got %v)", c.rule, c.line, ids(fs))
		}
	}
}

func TestRedact(t *testing.T) {
	v := join("gh", "p_", "abcdefghijklmnopqrstuvwxyz0123456789")
	if got := Redact(v); got != "ghp_…89" {
		t.Errorf("Redact = %q", got)
	}
	if got := Redact("short"); got != "sh…" {
		t.Errorf("Redact(short) = %q", got)
	}
}

func TestRulesFalsePositives(t *testing.T) {
	s := New(nil)
	lines := []string{
		join("aws_access_key_id = AK", "IAIOSFODNN7EX", "AMPLE"), // AWS's documented example
		join(`token = "gh`, `p_`, strings.Repeat("x", 36), `"`),
		join(`token = "gh`, `p_`, strings.Repeat("0", 36), `"`),
		`password = "correcthorsebatterystaple"`,       // no digits
		`api_key = os.Getenv("MY_SERVICE_API_KEY_V2")`, // a lookup, not a value
		`secret_name = "projects/my-proj/secrets/db-password/versions/latest"`,
		`token = "${{ secrets.DEPLOY_TOKEN }}"`,
		`if strings.HasPrefix(s, "-----BEGIN RSA PRIVATE KEY-----") {`,
		`const sum = "sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="`, // integrity hash, no secret name
		`author_email = "someone@example.com"`,
		`// Use sk-... for OpenAI keys and ghp_... for GitHub tokens.`,
		`token = "` + strings.Repeat("ab12", 8) + `"`, // low entropy
		`commit = "` + rnd(40, hexch) + `"`,           // a hash isn't a secret by name
	}
	for _, l := range lines {
		if fs := s.ScanText(l); len(fs) > 0 {
			t.Errorf("false positive in %q: %v", l, ids(fs))
		}
	}
}

func TestPrivateKey(t *testing.T) {
	s := New(nil)
	body := rnd(64, alnum+"+/")
	file := "-----BEGIN " + "RSA PRIVATE KEY-----\n" + body + "\n" + rnd(64, alnum) + "\n-----END RSA PRIVATE KEY-----\n"
	fs := s.ScanFile("deploy/id_rsa", []byte(file))
	if len(fs) != 1 || fs[0].RuleID != "private-key" || fs[0].Line != 1 || fs[0].Path != "deploy/id_rsa" {
		t.Fatalf("findings = %+v", fs)
	}
	if strings.Contains(fs[0].Redacted, body) || !strings.HasPrefix(fs[0].Redacted, "RSA PRIVATE KEY "+body[:4]) {
		t.Errorf("redacted = %q", fs[0].Redacted)
	}
	// Embedded in a JSON string with escaped newlines.
	js := `{"private_key": "-----BEGIN ` + `PRIVATE KEY-----\n` + body + `\n-----END PRIVATE KEY-----\n"}`
	if fs := s.ScanText(js); len(fs) == 0 || fs[0].RuleID != "private-key" {
		t.Errorf("JSON key: %v", ids(fs))
	}
	// Two different keys get different fingerprints.
	other := strings.Replace(file, body, rnd(64, alnum), 1)
	if fs2 := s.ScanFile("k2", []byte(other)); len(fs2) != 1 || fs2[0].Fingerprint == fs[0].Fingerprint {
		t.Errorf("second key: %+v", fs2)
	}
}

func TestBase64AndSplitLines(t *testing.T) {
	s := New(nil)
	gh := fakeGitHub()
	enc := base64.StdEncoding.EncodeToString([]byte("machine github.com login x password " + gh + "\n"))
	fs := s.ScanText("NETRC_B64=" + enc)
	if len(fs) != 1 || fs[0].RuleID != "github-pat" || fs[0].Encoded != "base64" || fs[0].Fingerprint != Fingerprint(gh) {
		t.Fatalf("base64: %+v", fs)
	}
	if fs[0].Secret() != enc {
		t.Error("base64 finding should mask the encoded text")
	}
	// URL-safe base64 without padding.
	enc2 := base64.RawURLEncoding.EncodeToString([]byte(`{"key":"` + fakeStripe() + `"}`))
	if fs := s.ScanText("cfg: " + enc2); len(fs) != 1 || fs[0].RuleID != "stripe-secret-key" {
		t.Errorf("base64url: %v", ids(fs))
	}

	key := fakeAnthropic()
	src := "package x\n\nconst key = \"" + key[:30] + "\" +\n\t\"" + key[30:70] + "\" +\n\t\"" + key[70:] + "\"\n"
	fs = s.ScanFile("x.go", []byte(src))
	if len(fs) != 1 || fs[0].RuleID != "anthropic-api-key" || fs[0].Line != 3 || fs[0].Encoded != "joined" || fs[0].Fingerprint != Fingerprint(key) {
		t.Fatalf("split literal: %+v", fs)
	}
	gh2 := fakeGitHub()
	sh := "export TOKEN=" + gh2[:12] + "\\\n" + gh2[12:] + "\n"
	if fs := s.ScanFile("env.sh", []byte(sh)); len(fs) != 1 || fs[0].RuleID != "github-pat" || fs[0].Fingerprint != Fingerprint(gh2) {
		t.Errorf("backslash continuation: %+v", fs)
	}
	// Lines that aren't adjacent (separate hunks) aren't joined.
	lines := []Line{{No: 3, Text: `x := "` + key[:40] + `" +`}, {No: 9, Text: `"` + key[40:] + `"`}}
	if fs := s.ScanLines("y.go", lines); len(fs) != 0 {
		t.Errorf("non-adjacent lines joined: %v", ids(fs))
	}
}

func TestAllowlistAndSkips(t *testing.T) {
	gh := fakeGitHub()
	al, err := ParseAllowlist([]byte("paths:\n  - testdata/**\n  - \"*.fixture\"\n  - docs/\nfingerprints:\n  - " + Fingerprint(gh)[:16] + "  # test token\n  - nothex!\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(al.Fingerprints) != 1 {
		t.Errorf("fingerprints = %v", al.Fingerprints)
	}
	for p, want := range map[string]bool{
		"testdata/a.txt": true, "pkg/testdata/a.txt": false, "testdata/deep/x/y": true,
		"a/b/c.fixture": true, "docs/x.md": true, "src/docs.go": false, "main.go": false,
	} {
		if got := al.PathAllowed(p); got != want {
			t.Errorf("PathAllowed(%q) = %v", p, got)
		}
	}
	s := New(al)
	other := fakeGitHub()
	data := []byte("a=" + gh + "\nb=" + other + "\n")
	fs := s.ScanFile("main.go", data)
	if len(fs) != 1 || fs[0].Fingerprint != Fingerprint(other) || fs[0].Line != 2 {
		t.Errorf("allowlisted fingerprint reported: %+v", fs)
	}
	if fs := s.ScanFile("testdata/x.env", data); fs != nil {
		t.Error("allowlisted path scanned")
	}
	if fs := s.ScanFile("x.bin", append([]byte{0, 1, 2}, data...)); fs != nil {
		t.Error("binary scanned")
	}
	if fs := s.ScanFile("go.sum", data); fs != nil {
		t.Error("lockfile scanned")
	}
	s.Limits.MaxFileBytes = 10
	if fs := s.ScanFile("big.txt", data); fs != nil {
		t.Error("large file scanned")
	}
	if _, err := ParseAllowlist([]byte("paths: [")); err == nil {
		t.Error("bad YAML accepted")
	}
}

func TestScanPatchLog(t *testing.T) {
	gh, aws := fakeGitHub(), fakeAWS()
	long := strings.Repeat("y", 20000) + gh
	log := "\x00" + strings.Repeat("a", 40) + "\n\n" +
		"diff --git a/app.env b/app.env\nnew file mode 100644\n--- /dev/null\n+++ b/app.env\n@@ -0,0 +1,2 @@\n+NAME=demo\n+TOKEN=" + gh + "\n" +
		"diff --git a/img.png b/img.png\nBinary files /dev/null and b/img.png differ\n" +
		"diff --git a/gone.txt b/gone.txt\n--- a/gone.txt\n+++ /dev/null\n" +
		"\x00" + strings.Repeat("b", 40) + "\n\n" +
		"diff --git \"a/sp ace.txt\" \"b/sp ace.txt\"\n--- \"a/sp ace.txt\"\n+++ \"b/sp ace.txt\"\n@@ -10,0 +11,1 @@\n+key " + aws + "\n" +
		"@@ -20 +21 @@\n-old\n+" + long + "\n"
	s := New(nil)
	fs, st, err := s.ScanPatchLog(strings.NewReader(log), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Commits != 2 || st.Truncated || len(fs) != 2 {
		t.Fatalf("stats %+v findings %+v", st, fs)
	}
	if fs[0].Path != "app.env" || fs[0].Line != 2 || fs[0].Commit != strings.Repeat("a", 40) || fs[0].RuleID != "github-pat" {
		t.Errorf("first = %+v", fs[0])
	}
	if fs[1].Path != "sp ace.txt" || fs[1].Line != 11 || fs[1].RuleID != "aws-access-key-id" {
		t.Errorf("second = %+v", fs[1])
	}
	// The token at the end of a 20 KB line is past the per-line cap.
	// Bounds: one commit only, or a byte cap.
	fs, st, _ = s.ScanPatchLog(strings.NewReader(log), 1, 0)
	if st.Commits != 1 || !st.Truncated || len(fs) != 1 {
		t.Errorf("commit cap: %+v %d", st, len(fs))
	}
	_, st, _ = s.ScanPatchLog(strings.NewReader(log), 0, 100)
	if !st.Truncated {
		t.Errorf("byte cap: %+v", st)
	}
}

func BenchmarkScanFile(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString(`	x := fmt.Sprintf("value %d for token handling and auth", i) // comment` + "\n")
	}
	data := []byte(sb.String())
	s := New(nil)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		s.ScanFile("big.go", data)
	}
}
