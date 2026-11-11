package web

import (
	"context"
	"log"
	"net/http"
	"time"

	"atelier/internal/store"
	"atelier/internal/tunnel"
)

// The remote access switch for the app API: the same tunnel as Settings →
// Remote access (tunnel.go), for the personal owner only. Through the tunnel
// itself it can only be turned off.

type appRemote struct {
	On        bool
	Mode      string
	Address   string
	Exposed   []string
	Connected int
}

func (s appRemote) json() map[string]any {
	return map[string]any{"on": s.On, "mode": s.Mode, "address": appStrOrNil(s.Address), "exposed": s.Exposed, "connected": s.Connected}
}

// appRemoteState is where remote access stands. Connected counts the
// visitors seen through the tunnel in the last five minutes.
func (a *App) appRemoteState(ctx context.Context) appRemote {
	s := appRemote{Mode: tunnel.ModeQuick, Exposed: []string{}}
	c, err := a.db.TunnelConfig(ctx)
	if err != nil {
		return s
	}
	s.On, s.Mode = c.Enabled, c.Mode
	if s.Mode == "" {
		s.Mode = tunnel.ModeQuick
	}
	for _, e := range []struct {
		on    bool
		class string
	}{{c.ExposeWeb, classWeb}, {c.ExposeGit, classGit}, {c.ExposeMCP, classMCP}, {c.ExposePages, classPages}, {c.ExposeAPI, classAPI},
		{a.connectTunnelExposed(ctx), classConnect}} {
		if e.on {
			s.Exposed = append(s.Exposed, e.class)
		}
	}
	if s.On {
		s.Address = a.tunnel().sup.Status().Address
		s.Connected, _ = a.db.TunnelVisitors(ctx, time.Now().Add(-5*time.Minute))
	}
	return s
}

// appRemoteOwner limits the remote endpoints to the personal owner.
func (a *App) appRemoteOwner(w http.ResponseWriter, r *http.Request, c *appCaller) bool {
	if c.owner(r.Context(), a) {
		return true
	}
	appError(w, http.StatusForbidden, "forbidden", "Only the studio's owner can see or change remote access.")
	return false
}

func (a *App) appRemoteGet(w http.ResponseWriter, r *http.Request, c *appCaller) {
	if !a.appRemoteOwner(w, r, c) {
		return
	}
	writeJSON(w, http.StatusOK, a.appRemoteState(r.Context()).json())
}

func (a *App) appRemoteSet(w http.ResponseWriter, r *http.Request, c *appCaller) {
	if !a.appRemoteOwner(w, r, c) {
		return
	}
	var in struct {
		On *bool `json:"on"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	if in.On == nil {
		appError(w, http.StatusUnprocessableEntity, "invalid_request", `Send {"on": true} or {"on": false}.`)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	m := a.tunnel()
	by := c.Who.Name
	if !*in.On {
		if isRemote(r) {
			// Answer first, then cut: closing the listener ends this connection too.
			m.on.Store(false)
			n, err := a.db.SetTunnelEnabled(ctx, false, by)
			if err != nil {
				logErr(r, err)
			}
			w.Header().Set("Connection", "close")
			writeJSON(w, http.StatusOK, a.appRemoteState(ctx).json())
			_ = http.NewResponseController(w).Flush()
			m.kill()
			log.Printf("tunnel: turned off by %s through the tunnel; %d remote session(s) revoked", by, n)
			a.appBroadcastRemote(ctx)
			return
		}
		m.kill()
		n, err := a.db.SetTunnelEnabled(ctx, false, by)
		if err != nil {
			a.appFail(w, r, err)
			return
		}
		log.Printf("tunnel: turned off by %s; %d remote session(s) revoked", by, n)
		a.appBroadcastRemote(ctx)
		writeJSON(w, http.StatusOK, a.appRemoteState(ctx).json())
		return
	}
	if isRemote(r) {
		appError(w, http.StatusForbidden, "remote_only_off", "Remote access can only be turned on from your home network. From here you can turn it off.")
		return
	}
	problem, err := a.tunnelTurnOn(ctx, by)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	if problem != "" {
		appError(w, http.StatusConflict, "remote_unavailable", problem)
		return
	}
	a.appBroadcastRemote(ctx)
	writeJSON(w, http.StatusOK, a.appRemoteState(ctx).json())
}

// appRemoteWho lists who has been coming through the tunnel in the last five
// minutes, one entry per address.
func (a *App) appRemoteWho(w http.ResponseWriter, r *http.Request, c *appCaller) {
	if !a.appRemoteOwner(w, r, c) {
		return
	}
	ctx := r.Context()
	sessions := []map[string]any{}
	if st := a.appRemoteState(ctx); st.On {
		a.tunnel().flush(ctx)
		entries, err := a.db.TunnelLog(ctx, 2000)
		if err != nil {
			a.appFail(w, r, err)
			return
		}
		cut := time.Now().Add(-5 * time.Minute)
		type sess struct {
			e     store.TunnelLogEntry // the newest entry
			since time.Time
			in    bool
		}
		byIP := map[string]*sess{}
		var order []string
		for _, e := range entries { // newest first
			if e.At.Before(cut) {
				break
			}
			s, ok := byIP[e.IP]
			if !ok {
				s = &sess{e: e}
				byIP[e.IP] = s
				order = append(order, e.IP)
			}
			s.since, s.in = e.At, s.in || e.SignedIn
		}
		for _, ip := range order {
			s := byIP[ip]
			sessions = append(sessions, map[string]any{"ip": ip, "country": appStrOrNil(s.e.Country), "signed_in": s.in,
				"class": s.e.Class, "since": appTime(s.since)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// appRemoteLink makes the one-time passkey link (Settings → Remote access):
// only while remote access is on, and only from home.
func (a *App) appRemoteLink(w http.ResponseWriter, r *http.Request, c *appCaller) {
	if !a.appRemoteOwner(w, r, c) {
		return
	}
	if isRemote(r) {
		appError(w, http.StatusForbidden, "remote_only_off", "A passkey link can only be made from your home network.")
		return
	}
	ctx := r.Context()
	host := a.tunnel().remoteHost()
	if st := a.appRemoteState(ctx); !st.On || host == "" {
		appError(w, http.StatusConflict, "remote_off", "Turn remote access on and wait for its address first.")
		return
	}
	code, err := a.db.CreateTunnelEnroll(ctx, c.Who.PersonID, host, tunnelEnrollTTL)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"url": tunnelURL(host) + "/settings/remote/enroll?code=" + code,
		"expires_in": int(tunnelEnrollTTL.Seconds())})
}
