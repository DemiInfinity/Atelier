package connect

// ActivityStreams / ForgeFed shapes. Outgoing documents are plain structs and
// maps; incoming ones are untrusted and read through DecodeJSON and the small
// accessors below, which accept the different ways ActivityPub servers write
// the same thing (a link as a string or an object, one value or a list).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Media types.
const (
	ContentType   = "application/activity+json"
	LDContentType = `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
	JRDType       = "application/jrd+json"
	Public        = "https://www.w3.org/ns/activitystreams#Public"
	MaxBody       = 1 << 20 // inbound documents larger than this are refused
)

// Context is the @context of every document Atelier publishes.
var Context = []any{
	"https://www.w3.org/ns/activitystreams",
	"https://w3id.org/security/v1",
	"https://forgefed.org/ns",
}

// PublicKey is an actor's publicKey.
type PublicKey struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	PublicKeyPem string `json:"publicKeyPem"`
}

// Endpoints holds the shared inbox.
type Endpoints struct {
	SharedInbox string `json:"sharedInbox,omitempty"`
}

// Actor is a published actor document: a Person, an Organization or a
// ForgeFed Repository.
type Actor struct {
	Context                   any        `json:"@context,omitempty"`
	ID                        string     `json:"id"`
	Type                      string     `json:"type"`
	PreferredUsername         string     `json:"preferredUsername,omitempty"`
	Name                      string     `json:"name,omitempty"`
	Summary                   string     `json:"summary,omitempty"`
	URL                       string     `json:"url,omitempty"`
	Inbox                     string     `json:"inbox"`
	Outbox                    string     `json:"outbox,omitempty"`
	Followers                 string     `json:"followers,omitempty"`
	Following                 string     `json:"following,omitempty"`
	Endpoints                 *Endpoints `json:"endpoints,omitempty"`
	PublicKey                 *PublicKey `json:"publicKey,omitempty"`
	AttributedTo              string     `json:"attributedTo,omitempty"`
	CloneURI                  []string   `json:"cloneUri,omitempty"`
	Published                 string     `json:"published,omitempty"`
	ManuallyApprovesFollowers bool       `json:"manuallyApprovesFollowers"`
	Discoverable              bool       `json:"discoverable"`
}

// Collection is an OrderedCollection (followers, following, outbox).
type Collection struct {
	Context      any    `json:"@context,omitempty"`
	ID           string `json:"id"`
	Type         string `json:"type"`
	TotalItems   int    `json:"totalItems"`
	OrderedItems []any  `json:"orderedItems"`
}

// Activity builds an outgoing activity.
func Activity(id, typ, actor string, object any, to, cc []string, published time.Time) map[string]any {
	m := map[string]any{"@context": Context, "id": id, "type": typ, "actor": actor, "object": object}
	if len(to) > 0 {
		m["to"] = to
	}
	if len(cc) > 0 {
		m["cc"] = cc
	}
	if !published.IsZero() {
		m["published"] = published.UTC().Format(time.RFC3339)
	}
	return m
}

// ---------- reading untrusted JSON ----------

// Doc is a decoded JSON object.
type Doc map[string]json.RawMessage

var errNotObject = errors.New("not a JSON object")

// DecodeJSON reads exactly one JSON object of at most max bytes, with nothing
// after it.
func DecodeJSON(body []byte, max int) (Doc, error) {
	if len(body) > max {
		return nil, errors.New("document too large")
	}
	if !utf8.Valid(body) {
		return nil, errors.New("document is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var d Doc
	if err := dec.Decode(&d); err != nil {
		return nil, errNotObject
	}
	if d == nil {
		return nil, errNotObject
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	return d, nil
}

// Str is a string field ("" if missing or not a string).
func (d Doc) Str(k string) string {
	var s string
	if raw, ok := d[k]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// ID is a link field: a string, an object's "id", or the first of a list.
func (d Doc) ID(k string) string { return idOf(d[k]) }

// IDs is a field holding one or more links.
func (d Doc) IDs(k string) []string {
	raw := d[k]
	if len(raw) == 0 {
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for i, it := range list {
			if i == 100 {
				break
			}
			if s := idOf(it); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if s := idOf(raw); s != "" {
		return []string{s}
	}
	return nil
}

// Obj is an embedded object (nil when the field is missing or only a link).
func (d Doc) Obj(k string) Doc {
	var o Doc
	if raw, ok := d[k]; ok && json.Unmarshal(raw, &o) == nil {
		return o
	}
	return nil
}

// Type is the "type" field; for a list of types, the first.
func (d Doc) Type() string {
	if s := d.Str("type"); s != "" {
		return s
	}
	var list []string
	if raw, ok := d["type"]; ok && json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		return list[0]
	}
	return ""
}

// Time parses an RFC 3339 field (zero if missing or malformed).
func (d Doc) Time(k string) time.Time {
	t, _ := time.Parse(time.RFC3339, d.Str(k))
	return t
}

// Int reads an integer field.
func (d Doc) Int(k string) int64 {
	var n json.Number
	if raw, ok := d[k]; ok && json.Unmarshal(raw, &n) == nil {
		v, _ := n.Int64()
		return v
	}
	return 0
}

func idOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		ID   string `json:"id"`
		Href string `json:"href"`
	}
	if json.Unmarshal(raw, &o) == nil {
		if o.ID != "" {
			return o.ID
		}
		return o.Href
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		return idOf(list[0])
	}
	return ""
}

// RemoteActor is what Atelier keeps of someone else's actor document.
type RemoteActor struct {
	ID            string
	Type          string
	Username      string
	Name          string
	Summary       string
	URL           string
	Inbox         string
	SharedInbox   string
	Outbox        string
	KeyID         string
	KeyPEM        string
	CloneURI      string
	AttributedTo  string
	ManualApprove bool
}

// Host is the actor's host (lowercase, with port if any).
func (ra RemoteActor) Host() string { return HostOf(ra.ID) }

// Handle is @user@host, or the host for actors without a username.
func (ra RemoteActor) Handle() string {
	if ra.Username == "" {
		return ra.Host()
	}
	return "@" + ra.Username + "@" + ra.Host()
}

// ParseActor reads an actor document fetched from want. It checks that the
// document's id is want, and that its inboxes and key live on the same host.
func ParseActor(d Doc, want string) (RemoteActor, error) {
	ra := RemoteActor{
		ID: d.Str("id"), Type: d.Type(), Username: d.Str("preferredUsername"), Name: d.Str("name"),
		Summary: d.Str("summary"), URL: d.ID("url"), Inbox: d.ID("inbox"), Outbox: d.ID("outbox"),
		AttributedTo: d.ID("attributedTo"),
	}
	if ep := d.Obj("endpoints"); ep != nil {
		ra.SharedInbox = ep.ID("sharedInbox")
	}
	if cl := d.IDs("cloneUri"); len(cl) > 0 {
		ra.CloneURI = cl[0]
	}
	var mab bool
	if raw, ok := d["manuallyApprovesFollowers"]; ok && json.Unmarshal(raw, &mab) == nil {
		ra.ManualApprove = mab
	}
	// publicKey: an object, or a list of them.
	if pk := d.Obj("publicKey"); pk != nil {
		ra.KeyID, ra.KeyPEM = pk.Str("id"), pk.Str("publicKeyPem")
		if o := pk.ID("owner"); o != "" && o != ra.ID {
			ra.KeyID, ra.KeyPEM = "", ""
		}
	} else {
		var list []Doc
		if raw, ok := d["publicKey"]; ok && json.Unmarshal(raw, &list) == nil {
			for _, pk := range list {
				if o := pk.ID("owner"); o == "" || o == ra.ID {
					ra.KeyID, ra.KeyPEM = pk.Str("id"), pk.Str("publicKeyPem")
					break
				}
			}
		}
	}
	if ra.ID == "" || ra.ID != want {
		return ra, errors.New("the actor document's id doesn't match its address")
	}
	if ra.Inbox == "" {
		return ra, errors.New("the actor has no inbox")
	}
	host := ra.Host()
	for _, u := range []string{ra.Inbox, ra.SharedInbox, ra.Outbox, ra.KeyID} {
		if u != "" && HostOf(u) != host {
			return ra, errors.New("the actor's inbox or key is on another host")
		}
	}
	if ra.URL != "" && !SafeLink(ra.URL) {
		ra.URL = ""
	}
	ra.Name, ra.Username = Clip(PlainText(ra.Name), 100), Clip(ra.Username, 100)
	ra.Summary = Clip(PlainText(ra.Summary), 500)
	return ra, nil
}

// HostOf is a URL's host, lowercased ("" if it doesn't parse).
func HostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// StripFragment drops #fragment from a URL (a keyId's #main-key).
func StripFragment(raw string) string {
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// SafeLink reports whether raw is a plain http(s) link without credentials.
func SafeLink(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// PlainText turns untrusted HTML or text into one plain line: tags dropped,
// a few entities decoded, whitespace collapsed. The result is still escaped
// when rendered; this only keeps markup out of what people read.
func PlainText(s string) string {
	if len(s) > 64<<10 {
		s = s[:64<<10]
	}
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
			b.WriteByte(' ')
		case r == '>' && in:
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	out := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'", "&nbsp;", " ", "&amp;", "&").Replace(b.String())
	return strings.Join(strings.Fields(out), " ")
}

// Clip shortens s to at most n runes.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
