package hub

import "testing"

// workers differ in what they can run: a known model goes only to a worker whose catalogue lists it, however idle
// and big the others are
func TestChooseOnlyWorkersThatListTheModel(t *testing.T) {
	flux := catalogModel{ID: "flux2-klein-4b", Kind: "image", Backend: "mflux", Size: 16 << 30}
	ollama := catalogModel{ID: "ollama/qwen3:8b", Kind: "chat", Backend: "openai", Here: true}
	busy := "m1.1"
	mac := &node{id: "mac", known: true, routes: true, specs: Specs{RAMGB: 64},
		state: WorkerState{Running: &busy, Queued: 3}, models: []catalogModel{flux}}
	linux := &node{id: "linux", known: true, routes: true, specs: Specs{RAMGB: 128, OS: "Linux"},
		models: []catalogModel{ollama}}
	nodes := []*node{linux, mac}

	if n, ok := choose(nodes, linux, flux, true, nil); !ok || n != mac {
		t.Errorf("flux went to %v (ok %v), want the Mac", n, ok)
	}
	if n, ok := choose(nodes, mac, ollama, true, nil); !ok || n != linux {
		t.Errorf("the Ollama model went to %v (ok %v), want the Linux PC", n, ok)
	}
	if _, ok := choose([]*node{linux}, linux, flux, true, nil); ok {
		t.Error("flux was given to a worker that cannot run it")
	}
	// an unknown model (no worker lists it) is not filtered: the worker answers for itself
	if _, ok := choose(nodes, mac, catalogModel{ID: "nope"}, false, nil); !ok {
		t.Error("an unknown model found no worker")
	}
}
