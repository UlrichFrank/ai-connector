package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Device is one signed-in client (a phone, a browser, the TUI).
type Device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	User     string    `json:"user"` // who uses it ("Philipp"); "" for devices signed in before users existed
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
	// Owner: signed in with the password. Owners can always invite and are the only ones that may change
	// other devices (PATCH /api/devices/{id}).
	Owner bool `json:"owner"`
	// CanInvite: may show a QR code that signs in another device. Always true for owners; off by default for
	// devices signed in with an invite, an owner turns it on.
	CanInvite bool `json:"canInvite"`
	// InvitedBy is the id of the device whose invite signed this one in ("" for owners). It may name a device
	// that has since been revoked: revoking a device leaves the devices it invited alone.
	InvitedBy string `json:"invitedBy"`
}

// mayInvite: this device may create invites.
func (d Device) mayInvite() bool { return d.Owner || d.CanInvite }

// stored is a device as it sits in devices.json: with the hash of its token, never the token itself.
type stored struct {
	Device
	Hash string `json:"hash"`
}

// seenEvery: lastSeen is written at most this often, so a busy client does not rewrite the file per request.
const seenEvery = time.Minute

// devices keeps the device tokens in <data>/devices.json.
type devices struct {
	path string

	mu     sync.Mutex
	byHash map[string]*stored
}

func openDevices(dir string) (*devices, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	d := &devices{path: filepath.Join(dir, "devices.json"), byHash: map[string]*stored{}}
	b, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*stored
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	// Devices from before users and invites have no "owner": they all signed in with the password, so they
	// are owners (and may invite); their user is "" until an owner sets it.
	var flags []struct {
		Owner *bool `json:"owner"`
	}
	if err := json.Unmarshal(b, &flags); err != nil {
		return nil, err
	}
	migrated := false
	for i, s := range list {
		if flags[i].Owner == nil {
			s.Owner, s.CanInvite, migrated = true, true, true
		}
		if s.Owner {
			s.CanInvite = true
		}
		d.byHash[s.Hash] = s
	}
	if migrated {
		if err := d.saveLocked(); err != nil {
			log.Printf("devices.json: migrating: %v", err) // best effort: it is migrated again next time
		}
	}
	return d, nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b) // never fails (crypto/rand panics instead)
	return base64.RawURLEncoding.EncodeToString(b)
}

// add signs in a new device and returns its token (only the caller ever sees it). invitedBy is "" for an
// owner (the password), else the id of the device whose invite was used.
func (d *devices) add(name, user, invitedBy string) (string, Device, error) {
	token := randomString(32)
	now := time.Now().UTC().Truncate(time.Second)
	owner := invitedBy == ""
	s := &stored{Device: Device{
		ID: randomString(9), Name: name, User: user, Created: now, LastSeen: now,
		Owner: owner, CanInvite: owner, InvitedBy: invitedBy,
	}, Hash: hashToken(token)}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byHash[s.Hash] = s
	if err := d.saveLocked(); err != nil {
		delete(d.byHash, s.Hash)
		return "", Device{}, err
	}
	return token, s.Device, nil
}

// lookup finds the device of a token (and notes that it was seen). The token is hashed first, so the map
// lookup never compares secrets directly.
func (d *devices) lookup(token string) (Device, bool) {
	if token == "" {
		return Device{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.byHash[hashToken(token)]
	if !ok {
		return Device{}, false
	}
	if now := time.Now().UTC(); now.Sub(s.LastSeen) >= seenEvery {
		s.LastSeen = now.Truncate(time.Second)
		d.saveLocked() // best effort: a missed lastSeen is not worth failing the request
	}
	return s.Device, true
}

func (d *devices) list() []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Device, 0, len(d.byHash))
	for _, s := range d.byHash {
		out = append(out, s.Device)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// get finds a device by id.
func (d *devices) get(id string) (Device, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.byHash {
		if s.ID == id {
			return s.Device, true
		}
	}
	return Device{}, false
}

// update changes a device by id and saves; false when there is none. Owners keep canInvite whatever change
// does.
func (d *devices) update(id string, change func(*Device)) (Device, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.byHash {
		if s.ID == id {
			before := s.Device
			change(&s.Device)
			s.ID = before.ID
			if s.Owner {
				s.CanInvite = true
			}
			if err := d.saveLocked(); err != nil {
				s.Device = before
				return Device{}, true, err
			}
			return s.Device, true, nil
		}
	}
	return Device{}, false, nil
}

// revoke removes a device by id; false when there is none.
func (d *devices) revoke(id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for h, s := range d.byHash {
		if s.ID == id {
			delete(d.byHash, h)
			return true, d.saveLocked()
		}
	}
	return false, nil
}

// saveLocked writes the file atomically.
func (d *devices) saveLocked() error {
	list := make([]*stored, 0, len(d.byHash))
	for _, s := range d.byHash {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	return writeFileAtomic(d.path, list)
}

// writeFileAtomic writes v as JSON to path (temp file + rename), so a crash never leaves half a file.
func writeFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	f, err := os.CreateTemp(filepath.Dir(path), "."+base+"-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
