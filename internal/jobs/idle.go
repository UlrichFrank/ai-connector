package jobs

import (
	"log"
	"time"
)

// Unloading after idle time: a backend whose model had no job for idle is unloaded, so a machine people also work on
// gets its memory back (20–45 GB for a large chat or video model) — unless the model is pinned (the default chat
// model, kept warm). The queue's running job's backend is never unloaded; the check holds the manager's lock, so no
// job starts on a backend while it is unloaded.

// UnloadIdle unloads every idle backend now; it returns the models it unloaded.
func (m *Manager) UnloadIdle(idle time.Duration, pinned map[string]bool, now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastUsed == nil {
		m.lastUsed = map[string]time.Time{}
	}
	var out []string
	for name, b := range m.Backends {
		loaded := b.Loaded()
		if loaded == "" || pinned[loaded] || (m.busy && m.current == name) {
			continue
		}
		last, ok := m.lastUsed[name]
		if !ok { // loaded before it was seen (a job beside the queue, a backend's own start): from now on
			m.lastUsed[name] = now
			continue
		}
		if now.Sub(last) >= idle {
			b.Unload()
			out = append(out, loaded)
		}
	}
	return out
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
