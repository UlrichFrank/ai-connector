package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// inviteTTL is how long a QR code signs in.
	inviteTTL = 10 * time.Minute
	// maxInvites caps the pending invites, so a device cannot make the map grow without end.
	maxInvites = 100
	// maxUser is the longest user name (in characters); longer ones are cut.
	maxUser = 40
)

// invite is a pending one-time code, kept by its hash.
type invite struct {
	by      string // the id of the device that made it
	expires time.Time
}

// invites keeps the pending codes in memory only: they live 10 minutes, so a hub restart losing them costs
// a new QR code at most.
type invites struct {
	mu     sync.Mutex
	byHash map[string]invite
	now    func() time.Time // time.Now; tests move the clock
}

func newInvites() *invites {
	return &invites{byHash: map[string]invite{}, now: time.Now}
}

// purgeLocked forgets the expired codes.
func (iv *invites) purgeLocked(now time.Time) {
	for h, in := range iv.byHash {
		if !now.Before(in.expires) {
			delete(iv.byHash, h)
		}
	}
}

// add makes a code for the device by (≥ 128 bits, base64url); false when too many are pending.
func (iv *invites) add(by string) (string, time.Time, bool) {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	now := iv.now()
	iv.purgeLocked(now)
	if len(iv.byHash) >= maxInvites {
		return "", time.Time{}, false
	}
	code := randomString(16)
	exp := now.Add(inviteTTL).UTC().Truncate(time.Second)
	iv.byHash[hashToken(code)] = invite{by: by, expires: exp}
	return code, exp, true
}

// use spends a code and returns the id of the device that made it; false when it is unknown, used or expired.
func (iv *invites) use(code string) (string, bool) {
	if code == "" {
		return "", false
	}
	iv.mu.Lock()
	defer iv.mu.Unlock()
	now := iv.now()
	iv.purgeLocked(now)
	h := hashToken(code)
	in, ok := iv.byHash[h]
	if !ok {
		return "", false
	}
	delete(iv.byHash, h)
	return in.by, true
}

// origin is the hub's public origin as the client sees it: behind traefik X-Forwarded-Proto and
// X-Forwarded-Host (the first entry of each), else the request's own scheme and host.
func origin(r *http.Request) string {
	first := func(v string) string { return strings.TrimSpace(strings.Split(v, ",")[0]) }
	scheme := strings.ToLower(first(r.Header.Get("X-Forwarded-Proto")))
	if scheme != "http" && scheme != "https" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := first(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// cleanUser trims a user name and cuts it to maxUser characters.
func cleanUser(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxUser {
		s = strings.TrimSpace(string([]rune(s)[:maxUser]))
	}
	return s
}

// ---------- endpoints ----------

// createInvite makes a QR code: owner devices, or devices that may invite.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	dev := deviceOf(r)
	if !dev.mayInvite() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "This device may not sign in other devices"})
		return
	}
	code, exp, ok := s.invites.add(dev.ID)
	if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate", "message": "Too many open invites, try again later"})
		return
	}
	link := "mlxlab://signin?hub=" + url.QueryEscape(origin(r)) + "&code=" + url.QueryEscape(code)
	log.Printf("invite made by %q (%s)", dev.Name, dev.ID)
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "url": link, "expires": exp.Format(time.RFC3339)})
}

// patchDevice changes another (or its own) device: canInvite and user. Owner devices only.
func (s *Server) patchDevice(w http.ResponseWriter, r *http.Request) {
	if !deviceOf(r).Owner {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "Only devices signed in with the password may change devices"})
		return
	}
	var body struct {
		CanInvite *bool   `json:"canInvite"`
		User      *string `json:"user"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	var user string
	if body.User != nil {
		if user = cleanUser(*body.User); user == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user", "message": "The user name is empty"})
			return
		}
	}
	dev, ok, err := s.devices.update(r.PathValue("id"), func(d *Device) {
		if body.CanInvite != nil {
			d.CanInvite = *body.CanInvite // owners keep it (see devices.update)
		}
		if body.User != nil {
			d.User = user
		}
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, dev)
}
