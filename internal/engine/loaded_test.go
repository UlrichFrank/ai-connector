package engine

import (
	"sync"
	"testing"
	"time"
)

// Asking a backend what is loaded must not wait for a running job (it holds the backend's lock for minutes: a
// download, a song). The web app and the iOS app ask on every connect.
func TestLoadedDoesNotWaitForAJob(t *testing.T) {
	chat, text, flux, music := &ChatBackend{}, &TextBackend{}, &FluxBackend{}, &MusicBackend{}
	for name, c := range map[string]struct {
		lock   *sync.Mutex
		loaded func() string
	}{
		"chat": {&chat.mu, chat.Loaded}, "text": {&text.mu, text.Loaded},
		"flux": {&flux.mu, flux.Loaded}, "music": {&music.mu, music.Loaded},
	} {
		c.lock.Lock() // a job is running
		done := make(chan struct{})
		go func() { c.loaded(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("%s: Loaded waits for the running job", name)
		}
		c.lock.Unlock()
	}
	music.state.set("ace-step-1.5-turbo")
	if music.Loaded() != "ace-step-1.5-turbo" {
		t.Fatal("status not reported")
	}
}
