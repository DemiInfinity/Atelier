package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"atelier/internal/store"
)

// Passkeys: WebAuthn registration in Settings → Passkeys, and "Sign in with a
// passkey" (a discoverable-credential login) on the sign-in page.
func init() {
	registerFeature(feature{
		name: "passkeys",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/passkeys", a.authed(a.passkeysPage))
			m.HandleFunc("POST /settings/passkeys/register/begin", a.passkeyAPI(a.passkeyRegisterBegin))
			m.HandleFunc("POST /settings/passkeys/register/finish", a.passkeyAPI(a.passkeyRegisterFinish))
			m.HandleFunc("POST /settings/passkeys/rename", a.authed(a.passkeyRename)) // id in the form
			m.HandleFunc("POST /settings/passkeys/delete", a.authed(a.passkeyDelete))
			m.HandleFunc("POST /auth/passkey/begin", a.passkeyLoginBegin)
			m.HandleFunc("POST /auth/passkey/finish", a.passkeyLoginFinish)
		},
	})
}

const (
	passkeyCookie   = "atelier_webauthn"
	passkeyTTL      = 5 * time.Minute
	passkeyMaxBody  = 64 << 10
	passkeyMaxNames = 80
)

// ---------- relying party ----------

// passkeyRP works out the WebAuthn relying party for a request: the RP ID is
// the host of ATELIER_PUBLIC_URL (else the request host) and the origin is the
// matching scheme://host[:port]. ok is false when the browser won't allow
// WebAuthn there (plain HTTP other than localhost, or an IP address).
func (a *App) passkeyRP(r *http.Request) (rpID, origin string, ok bool) {
	scheme, host := a.external(r)
	if a.cfg.PublicURL != "" {
		if u, err := url.Parse(a.cfg.PublicURL); err == nil && u.Host != "" {
			scheme, host = u.Scheme, u.Host
		}
	}
	origin = scheme + "://" + host
	rpID = host
	if h, _, err := net.SplitHostPort(host); err == nil {
		rpID = h
	}
	rpID = strings.ToLower(strings.Trim(rpID, "[]"))
	local := rpID == "localhost" || strings.HasSuffix(rpID, ".localhost")
	ok = rpID != "" && net.ParseIP(rpID) == nil && (scheme == "https" || local)
	return rpID, origin, ok
}

func (a *App) webAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	rpID, origin, ok := a.passkeyRP(r)
	if !ok {
		return nil, errPasskeysUnavailable
	}
	requireRK := true
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "Atelier",
		RPOrigins:     []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: &requireRK,
			UserVerification:   protocol.VerificationPreferred,
		},
		AttestationPreference: protocol.PreferNoAttestation,
	})
}

var errPasskeysUnavailable = errors.New("passkeys need HTTPS (or localhost)")

// ---------- the owner as a WebAuthn user ----------

type passkeyUser struct {
	id          []byte
	name        string
	display     string
	credentials []webauthn.Credential
	rows        map[string]int64 // credential ID → passkeys.id
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.id }
func (u *passkeyUser) WebAuthnName() string                       { return u.name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.display }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func (a *App) passkeyOwner(r *http.Request) (*passkeyUser, error) {
	ctx := r.Context()
	me, err := a.db.PersonalOwner(ctx)
	if err != nil {
		return nil, err
	}
	id, err := a.db.WebAuthnUserID(ctx)
	if err != nil {
		return nil, err
	}
	pks, err := a.db.Passkeys(ctx)
	if err != nil {
		return nil, err
	}
	u := &passkeyUser{id: id, name: me.Name, display: me.Label(), rows: map[string]int64{}}
	for _, p := range pks {
		var c webauthn.Credential
		if err := json.Unmarshal(p.Credential, &c); err != nil {
			log.Printf("passkeys: skipping unreadable credential %d: %v", p.ID, err)
			continue
		}
		u.credentials = append(u.credentials, c)
		u.rows[string(c.ID)] = p.ID
	}
	return u, nil
}

// ---------- ceremony state (server-side, keyed by a short-lived cookie) ----------

type passkeyCeremony struct {
	kind    string // "register" or "login"
	session webauthn.SessionData
	expires time.Time
}

var passkeyCeremonies = struct {
	sync.Mutex
	m map[string]passkeyCeremony
}{m: map[string]passkeyCeremony{}}

func (a *App) passkeySave(w http.ResponseWriter, r *http.Request, kind string, s *webauthn.SessionData) {
	tok := store.RandomToken(24)
	passkeyCeremonies.Lock()
	now := time.Now()
	for k, c := range passkeyCeremonies.m {
		if now.After(c.expires) {
			delete(passkeyCeremonies.m, k)
		}
	}
	if len(passkeyCeremonies.m) > 1000 { // unauthenticated begin requests can't grow this without bound
		for k := range passkeyCeremonies.m {
			delete(passkeyCeremonies.m, k)
			break
		}
	}
	passkeyCeremonies.m[store.Hash(tok)] = passkeyCeremony{kind: kind, session: *s, expires: now.Add(passkeyTTL)}
	passkeyCeremonies.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: passkeyCookie, Value: tok, Path: "/", HttpOnly: true, Secure: a.isHTTPS(r),
		SameSite: http.SameSiteStrictMode, MaxAge: int(passkeyTTL.Seconds()),
	})
}

// passkeyTake returns and forgets the ceremony started by this browser.
func (a *App) passkeyTake(w http.ResponseWriter, r *http.Request, kind string) (webauthn.SessionData, bool) {
	http.SetCookie(w, &http.Cookie{Name: passkeyCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.isHTTPS(r)})
	c, err := r.Cookie(passkeyCookie)
	if err != nil || c.Value == "" {
		return webauthn.SessionData{}, false
	}
	key := store.Hash(c.Value)
	passkeyCeremonies.Lock()
	cer, ok := passkeyCeremonies.m[key]
	delete(passkeyCeremonies.m, key)
	passkeyCeremonies.Unlock()
	if !ok || cer.kind != kind || time.Now().After(cer.expires) {
		return webauthn.SessionData{}, false
	}
	return cer.session, true
}

// ---------- JSON helpers ----------

func passkeyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func passkeyErr(w http.ResponseWriter, status int, msg string) {
	passkeyJSON(w, status, map[string]string{"error": msg})
}

// passkeyAPI is a.authed for JSON endpoints: it answers 401 instead of redirecting.
func (a *App) passkeyAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := a.sessionActor(r)
		if a.needsSetup(r.Context()) || !ok {
			passkeyErr(w, http.StatusUnauthorized, "Your session has ended. Sign in again.")
			return
		}
		h(w, withActor(r, who))
	}
}

// passkeyBody reads {"credential": {...}, ...} and returns the raw credential.
func passkeyBody(r *http.Request, extra any) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, passkeyMaxBody+1))
	if err != nil || len(body) > passkeyMaxBody {
		return nil, errors.New("request too large")
	}
	var env struct {
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Credential) == 0 {
		return nil, errors.New("missing credential")
	}
	if extra != nil {
		_ = json.Unmarshal(body, extra)
	}
	return env.Credential, nil
}

// ---------- settings page ----------

func (a *App) passkeysPage(w http.ResponseWriter, r *http.Request) {
	a.renderPasskeys(w, r, http.StatusOK, "", r.URL.Query().Get("saved") != "")
}

func (a *App) renderPasskeys(w http.ResponseWriter, r *http.Request, status int, errMsg string, saved bool) {
	pks, err := a.db.Passkeys(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rpID, origin, ok := a.passkeyRP(r)
	notice := ""
	if saved {
		notice = "saved"
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "passkeys_settings", page{Title: "Passkeys", Error: errMsg, Notice: notice, Data: map[string]any{
		"Passkeys":  pks,
		"Available": ok,
		"RPID":      rpID,
		"Origin":    origin,
	}})
}

func (a *App) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := a.webAuthn(r)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "Passkeys need HTTPS (or localhost). Open Atelier at its https:// address to add one.")
		return
	}
	u, err := a.passkeyOwner(r)
	if err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range u.credentials {
		exclude = append(exclude, c.Descriptor())
	}
	opts, sess, err := wa.BeginRegistration(u, webauthn.WithExclusions(exclude),
		webauthn.WithExtensions(webauthn.WithExtensionCredProps()))
	if err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Couldn't start passkey registration.")
		return
	}
	a.passkeySave(w, r, "register", sess)
	passkeyJSON(w, http.StatusOK, opts)
}

func (a *App) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.passkeyTake(w, r, "register")
	if !ok {
		passkeyErr(w, http.StatusBadRequest, "That took too long. Please try again.")
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "Passkeys need HTTPS (or localhost).")
		return
	}
	var extra struct {
		Name string `json:"name"`
	}
	raw, err := passkeyBody(r, &extra)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "The passkey response couldn't be read.")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "The passkey response couldn't be read.")
		return
	}
	u, err := a.passkeyOwner(r)
	if err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	cred, err := wa.CreateCredential(u, sess, parsed)
	if err != nil {
		log.Printf("passkeys: registration rejected: %v", err)
		passkeyErr(w, http.StatusBadRequest, "The passkey couldn't be verified. Please try again.")
		return
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	name := passkeyName(extra.Name)
	if name == "" {
		name = "Passkey"
	}
	act := a.actor(r)
	if _, err := a.db.AddPasskey(r.Context(), cred.ID, name, blob, act.Name, act.Kind); err != nil {
		if errors.Is(err, store.ErrKeyExists) {
			passkeyErr(w, http.StatusConflict, "That passkey is already registered.")
			return
		}
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	passkeyJSON(w, http.StatusOK, map[string]string{"redirect": "/settings/passkeys?saved=1"})
}

func passkeyName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > passkeyMaxNames {
		s = string(r[:passkeyMaxNames])
	}
	return s
}

func (a *App) passkeyRename(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	name := passkeyName(r.FormValue("name"))
	if name == "" {
		a.renderPasskeys(w, r, http.StatusUnprocessableEntity, "Give the passkey a name.", false)
		return
	}
	if err := a.db.RenamePasskey(r.Context(), id, name); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/passkeys?saved=1", http.StatusSeeOther)
}

func (a *App) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.db.DeletePasskey(r.Context(), id); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/passkeys?saved=1", http.StatusSeeOther)
}

// ---------- sign in with a passkey ----------

func (a *App) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if a.needsSetup(r.Context()) {
		passkeyErr(w, http.StatusConflict, "Atelier isn't set up yet.")
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "Passkeys need HTTPS (or localhost). Use your password instead.")
		return
	}
	if pks, err := a.db.Passkeys(r.Context()); err == nil && len(pks) == 0 {
		passkeyErr(w, http.StatusNotFound, "No passkeys have been added yet. Sign in with your password, then add one in Settings → Passkeys.")
		return
	}
	opts, sess, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Couldn't start passkey sign-in.")
		return
	}
	a.passkeySave(w, r, "login", sess)
	passkeyJSON(w, http.StatusOK, opts)
}

func (a *App) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ip := a.clientIP(r)
	if !a.lim.allow(ip) {
		passkeyErr(w, http.StatusTooManyRequests, "Too many attempts. Wait a minute and try again.")
		return
	}
	sess, ok := a.passkeyTake(w, r, "login")
	if !ok {
		passkeyErr(w, http.StatusBadRequest, "That took too long. Please try again.")
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "Passkeys need HTTPS (or localhost).")
		return
	}
	var extra struct {
		Next string `json:"next"`
	}
	raw, err := passkeyBody(r, &extra)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "The passkey response couldn't be read.")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		passkeyErr(w, http.StatusBadRequest, "The passkey response couldn't be read.")
		return
	}
	owner, err := a.passkeyOwner(r)
	if err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		if !bytes.Equal(userHandle, owner.id) {
			return nil, errors.New("unknown user handle")
		}
		return owner, nil
	}
	_, cred, err := wa.ValidatePasskeyLogin(handler, sess, parsed)
	if err != nil {
		log.Printf("passkeys: sign-in failed from %s: %v", ip, err)
		passkeyErr(w, http.StatusUnauthorized, "That passkey isn't registered with this Atelier.")
		return
	}
	if cred.Authenticator.CloneWarning {
		log.Printf("passkeys: sign-in refused from %s: signature counter went backwards (possible cloned authenticator)", ip)
		passkeyErr(w, http.StatusUnauthorized, "That passkey couldn't be verified.")
		return
	}
	if row, ok := owner.rows[string(cred.ID)]; ok {
		if blob, err := json.Marshal(cred); err == nil {
			_ = a.db.UsePasskey(r.Context(), row, blob)
		}
	}
	var personID int64
	if row, ok := owner.rows[string(cred.ID)]; ok {
		personID, _ = a.db.PasskeyPerson(r.Context(), row)
	}
	if personID == 0 {
		passkeyErr(w, http.StatusUnauthorized, "That passkey isn't registered with this Atelier.")
		return
	}
	a.lim.reset(ip)
	if err := a.startSessionFor(w, r, personID); err != nil {
		logErr(r, err)
		passkeyErr(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	passkeyJSON(w, http.StatusOK, map[string]string{"redirect": safeNext(extra.Next)})
}
