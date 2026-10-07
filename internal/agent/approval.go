package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotWaiting: no call of that id waits for a decision (it was decided, timed out, or never asked).
var ErrNotWaiting = errors.New("no such call waits for approval")

// Gate is where the calls of one running job wait for the user's decision. The job manager makes one per job and
// passes it in the job's context (WithGate); POST /api/jobs/{id}/approve ends up in Decide.
type Gate struct {
	mu      sync.Mutex
	waiting map[string]chan bool
}

// NewGate makes an empty gate.
func NewGate() *Gate { return &Gate{waiting: map[string]chan bool{}} }

// Wait registers the call, then announces it (the "approval" event: registered first, so a quick answer is never
// lost), and waits for the decision. No decision within timeout: not allowed (timedOut true). err is the context's
// when the job is canceled.
func (g *Gate) Wait(ctx context.Context, call string, timeout time.Duration, announce func()) (allow, timedOut bool, err error) {
	ch := make(chan bool, 1)
	g.mu.Lock()
	g.waiting[call] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.waiting, call)
		g.mu.Unlock()
	}()
	if announce != nil {
		announce()
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case a := <-ch:
		return a, false, nil
	case <-t.C:
		return false, true, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
}

// Decide answers a waiting call.
func (g *Gate) Decide(call string, allow bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.waiting[call]
	if !ok {
		return ErrNotWaiting
	}
	delete(g.waiting, call)
	ch <- allow // buffered: never blocks
	return nil
}

// Waiting lists the calls that wait (for tests and the UI after a reload).
func (g *Gate) Waiting() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for id := range g.waiting {
		out = append(out, id)
	}
	return out
}

type gateKey struct{}

// WithGate passes a job's gate to the loop.
func WithGate(ctx context.Context, g *Gate) context.Context {
	return context.WithValue(ctx, gateKey{}, g)
}

// GateFrom is the gate in the context (nil: nobody can approve, e.g. `mlxlab run`).
func GateFrom(ctx context.Context) *Gate {
	g, _ := ctx.Value(gateKey{}).(*Gate)
	return g
}
