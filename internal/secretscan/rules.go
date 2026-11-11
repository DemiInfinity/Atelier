// Package secretscan finds credentials in code, diffs and logs.
//
// It is used by push protection (the pre-receive hook), by the history scan
// and by Actions log scanning. Every rule is a regular expression with a
// cheap keyword prefilter, and every match must also pass placeholder and
// entropy checks so that examples ("AKIAIOSFODNN7EXAMPLE", "ghp_xxxx…") and
// identifiers don't raise alerts. Secret values never leave this package in
// full except through Finding.Secret, which callers use only to mask logs:
// alerts store the fingerprint (a hash) and a redacted form.
package secretscan

import (
	"math"
	"regexp"
	"strings"
)

// Rule is one kind of credential.
type Rule struct {
	ID   string
	Name string
	re   *regexp.Regexp
	// group is the capture group holding the secret (0 = the whole match).
	group int
	// keywords: at least one must appear in the lower-cased line for the
	// regular expression to run at all (a fast prefilter).
	keywords []string
	// body is a prefix stripped before the entropy check ("ghp_").
	minEntropy float64
	// generic rules are noisier: they only run on plain lines (not on decoded
	// base64 or joined lines) and need stronger evidence.
	generic bool
	pem     bool
}

func rule(id, name, re string, group int, minEntropy float64, keywords ...string) *Rule {
	return &Rule{ID: id, Name: name, re: regexp.MustCompile(re), group: group, keywords: keywords, minEntropy: minEntropy}
}

// DefaultRules is the built-in rule set.
var DefaultRules = []*Rule{
	// Atelier's own tokens (store.TokenPrefix and the Actions token prefixes).
	rule("atelier-access-token", "Atelier access token", `\batl_[0-9a-f]{40}\b`, 0, 3.0, "atl_"),
	rule("atelier-runner-token", "Atelier runner token", `\batr_[0-9a-f]{64}\b`, 0, 3.0, "atr_"),
	rule("atelier-job-token", "Atelier Actions job token", `\batj_[0-9a-f]{64}\b`, 0, 3.0, "atj_"),
	rule("atelier-registration-token", "Atelier runner registration token", `\batreg_[0-9a-f]{48}\b`, 0, 3.0, "atreg_"),

	// GitHub.
	rule("github-pat", "GitHub personal access token", `\bghp_[A-Za-z0-9]{36}\b`, 0, 3.5, "ghp_"),
	rule("github-fine-grained-pat", "GitHub fine-grained token", `\bgithub_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59}\b`, 0, 3.5, "github_pat_"),
	rule("github-oauth", "GitHub OAuth token", `\bgho_[A-Za-z0-9]{36}\b`, 0, 3.5, "gho_"),
	rule("github-app-token", "GitHub app token", `\b(?:ghu|ghs)_[A-Za-z0-9]{36}\b`, 0, 3.5, "ghu_", "ghs_"),
	rule("github-refresh-token", "GitHub refresh token", `\bghr_[A-Za-z0-9]{36,76}\b`, 0, 3.5, "ghr_"),

	// AI providers. Anthropic first: OpenAI's pattern must not claim sk-ant-.
	rule("anthropic-api-key", "Anthropic API key", `\bsk-ant-(?:api|admin|oat)\d{2}-[A-Za-z0-9_\-]{80,120}`, 0, 4.0, "sk-ant-"),
	rule("openai-api-key", "OpenAI API key",
		`\b(sk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{40,200}|sk-[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20}|sk-[A-Za-z0-9]{48})(?:[^A-Za-z0-9_\-]|$)`, 1, 4.0, "sk-"),

	// Cloud.
	rule("aws-access-key-id", "AWS access key ID", `\b((?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16})\b`, 1, 3.0, "akia", "asia", "abia", "acca"),
	rule("aws-secret-access-key", "AWS secret access key",
		`(?i)aws.{0,24}?(?:secret|private).{0,24}?(?:key)?["']?\s*(?:[:=]|=>|:=)\s*["']?([A-Za-z0-9/+]{40})(?:["'\s,;]|$)`, 1, 4.0, "aws"),
	rule("google-api-key", "Google API key", `\bAIza[0-9A-Za-z_\-]{35}\b`, 0, 3.5, "aiza"),

	// Chat and payments.
	rule("slack-token", "Slack token", `\bxox[abposr]-(?:[0-9]{8,}-){1,3}[A-Za-z0-9]{16,}\b`, 0, 3.0, "xox"),
	rule("slack-webhook", "Slack webhook URL", `https://hooks\.slack\.com/services/T[A-Z0-9]{8,}/B[A-Z0-9]{8,}/[A-Za-z0-9]{24}`, 0, 3.0, "hooks.slack.com"),
	rule("stripe-secret-key", "Stripe live secret key", `\b(?:sk|rk)_live_[0-9A-Za-z]{24,99}\b`, 0, 3.5, "sk_live_", "rk_live_"),

	// Private keys (the header line; the body line, if any, makes the fingerprint unique).
	{ID: "private-key", Name: "Private key", re: regexp.MustCompile(`-----BEGIN ((?:RSA |EC |DSA |OPENSSH |ENCRYPTED |PGP )?PRIVATE KEY(?: BLOCK)?)-----`),
		group: 1, keywords: []string{"private key"}, pem: true},

	// Generic assignments of high-entropy strings to secret-sounding names.
	{ID: "generic-secret", Name: "Generic secret", generic: true, minEntropy: 4.2, group: 1,
		keywords: []string{"secret", "token", "passw", "api_key", "apikey", "api-key", "access_key", "accesskey", "auth", "credential", "private_key"},
		re:       regexp.MustCompile(`(?i)(?:secret|token|passw(?:or)?d|api[_-]?key|access[_-]?key|auth[_-]?key|credentials?|private[_-]?key)[A-Za-z0-9_\-]{0,20}["']?\s*(?:[:=]|=>|:=)\s*["']([A-Za-z0-9+/_\-=.~]{24,200})["']`)},
}

// RuleByID returns a built-in rule.
func RuleByID(id string) *Rule {
	for _, r := range DefaultRules {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// placeholders mark example values that aren't real secrets.
var placeholders = []string{"example", "xxxxxx", "placeholder", "dummy", "changeme", "your_", "your-", "redacted", "*****", "<", "${", "{{", "$(", "000000", "123456", "abcdef0123", "insert", "replace"}

func looksPlaceholder(s string) bool {
	l := strings.ToLower(s)
	for _, p := range placeholders {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	e := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			e -= p * math.Log2(p)
		}
	}
	return e
}

// tokenBody strips a known prefix ("ghp_", "sk-ant-api03-") before measuring entropy.
func tokenBody(s string) string {
	if i := strings.LastIndexAny(s[:min(len(s), 16)], "_-"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// plausible reports whether a match looks like a real secret.
func (r *Rule) plausible(secret string) bool {
	if r.pem {
		return true
	}
	if looksPlaceholder(secret) {
		return false
	}
	body := tokenBody(secret)
	if r.generic {
		body = secret
		hasDigit, hasLetter := false, false
		for i := 0; i < len(body); i++ {
			c := body[i]
			hasDigit = hasDigit || c >= '0' && c <= '9'
			hasLetter = hasLetter || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		}
		if !hasDigit || !hasLetter {
			return false
		}
		// Hex can't exceed 4 bits per character: judge long hex strings on their own scale.
		if isHex(body) {
			return len(body) >= 32 && entropy(body) >= 3.6
		}
		// Dotted identifiers and paths aren't secrets.
		if strings.Count(body, ".") > 2 || strings.Count(body, "/") > 3 {
			return false
		}
	}
	return entropy(body) >= r.minEntropy
}
