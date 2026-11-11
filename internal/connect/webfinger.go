package connect

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// JRD is a WebFinger answer (RFC 7033).
type JRD struct {
	Subject string    `json:"subject"`
	Aliases []string  `json:"aliases,omitempty"`
	Links   []JRDLink `json:"links"`
}

// JRDLink is one link in a JRD.
type JRDLink struct {
	Rel  string `json:"rel"`
	Type string `json:"type,omitempty"`
	Href string `json:"href,omitempty"`
}

// SelfLink is the ActivityPub actor a JRD points at ("" if none).
func (j JRD) SelfLink() string {
	for _, l := range j.Links {
		if l.Rel == "self" && (l.Type == ContentType || strings.HasPrefix(l.Type, "application/ld+json")) {
			return l.Href
		}
	}
	return ""
}

var (
	userRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})(:[0-9]{1,5})?$`)
)

// ParseHandle reads @user@host, user@host or acct:user@host.
func ParseHandle(s string) (user, host string, err error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "acct:")
	s = strings.TrimPrefix(s, "@")
	user, host, ok := strings.Cut(s, "@")
	if !ok || !userRe.MatchString(user) || !hostRe.MatchString(host) || strings.Contains(host, "..") {
		return "", "", errors.New("use the form @name@studio.example")
	}
	return user, strings.ToLower(host), nil
}

// WebFingerURL is where to look up user@host.
func WebFingerURL(scheme, user, host string) string {
	q := url.Values{"resource": {"acct:" + user + "@" + host}}
	return scheme + "://" + host + "/.well-known/webfinger?" + q.Encode()
}
