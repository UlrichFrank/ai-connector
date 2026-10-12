package hub

// Linking an app (docs/api.md, "Linking an app (device flow)"): an app gets its own client token without anyone
// pasting it, in the manner of RFC 8628. It asks for a link (POST /api/link/start) and shows the PIN; a person
// signed in to the hub opens <hub>/pin, types the PIN and approves; the app, polling POST /api/link/token, picks
// the token up once.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/consoleui"
)

const (
	// linkTTL is how long a PIN waits for its approval (and the approved token for its pickup).
	linkTTL = 10 * time.Minute
	// linkInterval is how often the app polls, in seconds.
	linkInterval = 5
	// maxLinks caps the pending links, so the open start endpoint cannot make the map grow without end.
	maxLinks = 100
	// pinDigits: the PIN is typed on a phone, so it is short and numeric; the TTL, the cap on pending links and the
	// per-device rate limit on lookups keep guessing one hopeless.
	pinDigits = 6
)

// link is a pending request, kept by the hash of its device code.
type link struct {
	name     string
	pin      string // digits only
	expires  time.Time
	lastPoll time.Time
	denied   bool
	token    string // set on approval, for the one pickup
	client   Client
}

// decided: approved or denied, so it is no longer shown for approval.
func (l *link) decided() bool { return l.denied || l.token != "" }

// links keeps the pending requests in memory only (like invites): a hub restart costs a new PIN at most.
type links struct {
	mu     sync.Mutex
	byHash map[string]*link
	byPIN  map[string]string      // PIN → hash of the device code
	now    func() time.Time       // time.Now; tests move the clock
	pin    func() (string, error) // newPIN; tests pick the PIN
}

func newLinks() *links {
	return &links{byHash: map[string]*link{}, byPIN: map[string]string{}, now: time.Now, pin: newPIN}
}

// newPIN is pinDigits random digits, uniform (crypto/rand).
func newPIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", pinDigits, n.Int64()), nil
}

// normalizePIN keeps the digits only: "123 456", "123-456" and "123456" are the same PIN.
func normalizePIN(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatPIN shows a PIN as the app does: "123 456".
func formatPIN(pin string) string {
	if len(pin) != pinDigits {
		return pin
	}
	return pin[:3] + " " + pin[3:]
}

func (ls *links) purgeLocked(now time.Time) {
	for h, l := range ls.byHash {
		if !now.Before(l.expires) {
			delete(ls.byHash, h)
			delete(ls.byPIN, l.pin)
		}
	}
}

// add makes a link for the app name: its device code (≥ 128 bits, base64url) and a PIN no pending link has;
// ok=false when too many are pending.
func (ls *links) add(name string) (code string, l link, ok bool, err error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	now := ls.now()
	ls.purgeLocked(now)
	if len(ls.byHash) >= maxLinks {
		return "", link{}, false, nil
	}
	var pin string
	for {
		if pin, err = ls.pin(); err != nil {
			return "", link{}, false, err
		}
		if _, taken := ls.byPIN[pin]; !taken {
			break
		}
	}
	code = randomString(16)
	h := hashToken(code)
	e := &link{name: name, pin: pin, expires: now.Add(linkTTL).UTC().Truncate(time.Second)}
	ls.byHash[h] = e
	ls.byPIN[pin] = h
	return code, *e, true, nil
}

// pendingLocked finds the undecided link of a PIN (in any spelling).
func (ls *links) pendingLocked(pin string) (*link, string, bool) {
	ls.purgeLocked(ls.now())
	h, ok := ls.byPIN[normalizePIN(pin)]
	if !ok {
		return nil, "", false
	}
	l := ls.byHash[h]
	if l.decided() {
		return nil, "", false
	}
	return l, h, true
}

func (ls *links) get(pin string) (link, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	l, _, ok := ls.pendingLocked(pin)
	if !ok {
		return link{}, false
	}
	return *l, true
}

// approve hands the undecided link of a PIN to mk, which makes its client; false when there is none.
func (ls *links) approve(pin string, mk func(name string) (string, Client, error)) (Client, bool, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	l, _, ok := ls.pendingLocked(pin)
	if !ok {
		return Client{}, false, nil
	}
	token, c, err := mk(l.name)
	if err != nil {
		return Client{}, true, err
	}
	l.token, l.client = token, c
	return c, true, nil
}

// deny turns the undecided link of a PIN down; the app learns it on its next poll.
func (ls *links) deny(pin string) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	l, _, ok := ls.pendingLocked(pin)
	if ok {
		l.denied = true
	}
	return ok
}

// poll answers the app: the token once approved (the link is gone then), else the RFC 8628 error.
func (ls *links) poll(code string) (string, Client, string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	now := ls.now()
	ls.purgeLocked(now)
	h := hashToken(code)
	l, ok := ls.byHash[h]
	if code == "" || !ok {
		return "", Client{}, "expired_token"
	}
	if l.token != "" || l.denied {
		delete(ls.byHash, h)
		delete(ls.byPIN, l.pin)
		if l.denied {
			return "", Client{}, "access_denied"
		}
		return l.token, l.client, ""
	}
	// lenient: a second early is fine, the app's timer is not exact
	last := l.lastPoll
	l.lastPoll = now
	if !last.IsZero() && now.Sub(last) < (linkInterval-1)*time.Second {
		return "", Client{}, "slow_down"
	}
	return "", Client{}, "authorization_pending"
}

// publicBase is the hub's address for people: MLXLAB_PUBLIC_URL, else the origin the request came to.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	return origin(r)
}

// ---------- endpoints ----------

// startLink is the app's first call (no auth, rate-limited per IP): a device code to poll with and a PIN to show.
func (s *Server) startLink(w http.ResponseWriter, r *http.Request) {
	if !s.linkStarts.allow(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate", "message": "Too many link requests, try again in a minute"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name", "message": "The application needs a name"})
		return
	}
	if len(name) > 100 {
		name = name[:100]
	}
	code, l, ok, err := s.links.add(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate", "message": "Too many open link requests, try again later"})
		return
	}
	base := s.publicBase(r)
	log.Printf("link requested: %q from %s", name, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               code,
		"user_code":                 formatPIN(l.pin),
		"verification_uri":          base + "/pin",
		"verification_uri_complete": base + "/pin/" + l.pin,
		"expires_in":                int(linkTTL / time.Second),
		"interval":                  linkInterval,
	})
}

// linkToken is the app's poll (no auth: the device code is the secret).
func (s *Server) linkToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	token, c, errCode := s.links.poll(strings.TrimSpace(body.DeviceCode))
	if errCode != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errCode})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "client": c})
}

// linkLookupAllowed rate-limits the PIN lookups per device: a PIN is guessable, a device's guesses are few.
func (s *Server) linkLookupAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.linkLookups.allow(deviceOf(r).ID) {
		return true
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate", "message": "Too many PINs tried, try again in a minute"})
	return false
}

// getLink shows a pending link to the person about to approve it.
func (s *Server) getLink(w http.ResponseWriter, r *http.Request) {
	if !s.linkLookupAllowed(w, r) {
		return
	}
	l, ok := s.links.get(r.PathValue("code"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": l.name, "userCode": formatPIN(l.pin), "expires": l.expires.Format(time.RFC3339)})
}

// decideLink approves a link (its client acts for the approving user, so their claude-code workers run it) or
// denies it.
func (s *Server) decideLink(w http.ResponseWriter, r *http.Request) {
	if !s.linkLookupAllowed(w, r) {
		return
	}
	var body struct {
		Approve *bool `json:"approve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	if body.Approve == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": "approve is missing"})
		return
	}
	dev, pin := deviceOf(r), r.PathValue("code")
	if !*body.Approve {
		if !s.links.deny(pin) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		log.Printf("link denied by %q", dev.Name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	c, ok, err := s.links.approve(pin, func(name string) (string, Client, error) { return s.clients.add(name, dev.User) })
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	log.Printf("client made: %q (%s) by %q, owner %q (linked)", c.Name, c.ID, dev.Name, c.Owner)
	writeJSON(w, http.StatusOK, map[string]any{"client": c})
}

// pinPage is the short address the app shows (<hub>/pin, <hub>/pin/123456): it leads to the console's link page,
// on every host name. Anything but a PIN after /pin/ leads to the empty page.
func pinPage(w http.ResponseWriter, r *http.Request) {
	to := consoleui.Prefix + "#/link"
	if rest, ok := strings.CutPrefix(r.URL.Path, "/pin/"); ok && rest != "" {
		if pin := normalizePIN(rest); len(pin) == pinDigits && strings.Trim(rest, "0123456789 -") == "" {
			to += "/" + pin
		}
	}
	http.Redirect(w, r, to, http.StatusFound)
}
