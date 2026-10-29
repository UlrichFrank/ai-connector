package jobs

import (
	"log"
	"strings"
	"time"
)

// Unloading after idle time: a backend whose model had no job for idle is unloaded, so a machine people also work on
// gets its memory back (20–45 GB for a large chat or video model) — unless the model is pinned (the default chat
// model, kept warm). Nothing is unloaded while a job runs (a job may use other backends than its own: the avatar
// speaks through the speech backend). The candidates are chosen under the manager's lock and marked; the unloading
// happens outside it (a backend's Unload waits for the backend's own lock, which a running call may hold while it
// reports to the manager); a job for a backend being unloaded waits until it is done.

// Touch notes that a backend was used outside the queue (a direct embed or classify): its idle time begins again.
func (m *Manager) Touch(backend string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastUsed == nil {
		m.lastUsed = map[string]time.Time{}
	}
	m.lastUsed[backend] = time.Now()
}

// UnloadIdle unloads every idle backend now; it returns the models it unloaded.
func (m *Manager) UnloadIdle(idle time.Duration, pinned map[string]bool, now time.Time) []string {
	m.mu.Lock()
	if m.lastUsed == nil {
		m.lastUsed = map[string]time.Time{}
	}
	if m.unloading == nil {
		m.unloading = map[string]chan struct{}{}
	}
	type candidate struct {
		name, model string
		done        chan struct{}
	}
	var cs []candidate
	if !m.busy && m.beside == 0 {
		for name, b := range m.Backends {
			loaded := b.Loaded()
			if loaded == "" || anyPinned(loaded, pinned) || m.unloading[name] != nil {
				continue
			}
			last, ok := m.lastUsed[name]
			if !ok { // loaded before it was seen (a backend's own start): from now on
				m.lastUsed[name] = now
				continue
			}
			if now.Sub(last) >= idle {
				c := candidate{name, loaded, make(chan struct{})}
				m.unloading[name] = c.done
				cs = append(cs, c)
			}
		}
	}
	m.mu.Unlock()
	var out []string
	for _, c := range cs {
		m.Backends[c.name].Unload()
		m.mu.Lock()
		delete(m.unloading, c.name)
		delete(m.lastUsed, c.name) // loaded again later, its idle time begins when it is first seen
		m.mu.Unlock()
		close(c.done)
		out = append(out, c.model)
	}
	return out
}

// waitUnloaded waits (with the lock held on return) until the backend is not being unloaded. Called with m.mu held.
func (m *Manager) waitUnloaded(backend string) {
	for {
		done := m.unloading[backend]
		if done == nil {
			return
		}
		m.mu.Unlock()
		<-done
		m.mu.Lock()
	}
}

// WatchIdle unloads idle backends until stop is closed (idle 0: never). It looks every quarter of idle, at most
// every 30 s.
func (m *Manager) WatchIdle(idle time.Duration, pinned []string, stop <-chan struct{}) {
	if idle <= 0 {
		return
	}
	pin := map[string]bool{}
	for _, p := range pinned {
		pin[p] = true
	}
	every := min(idle/4, 30*time.Second)
	t := time.NewTicker(max(every, time.Second))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			for _, id := range m.UnloadIdle(idle, pin, now) {
				log.Printf("unloaded %s: no job for %s", id, idle)
			}
		}
	}
}

// anyPinned: a model the backend has loaded is pinned (a backend with several says them separated by ", ": the
// embedder and the classifier).
func anyPinned(loaded string, pinned map[string]bool) bool {
	for _, id := range strings.Split(loaded, ", ") {
		if pinned[id] {
			return true
		}
	}
	return false
}
