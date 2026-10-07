package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

// fetch makes sure the files of m are in the store (downloads resume where they stopped; files the Hugging Face
// cache already has are linked) and reports the bytes as progress: step and total are megabytes.
func fetch(ctx context.Context, store *models.Store, m models.Model, emit Emit) (string, error) {
	total := store.Missing(m)
	if total == 0 {
		return store.Path(m), nil
	}
	s := *store
	var done atomic.Int64
	s.Bytes = func(n int64) { done.Add(n) }
	s.Progress = func(format string, a ...any) { emit.Log(fmt.Sprintf(format, a...)) }
	report := func() {
		d := min(done.Load(), total)
		emit.Status(fmt.Sprintf("fetching %s: %s of %s", m.Repo, models.Human(d), models.Human(total)))
		emit.Progress(int(d>>20), int(total>>20))
	}
	report()
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				report()
			}
		}
	}()
	dir, err := s.Ensure(ctx, m)
	close(stop)
	emit.Progress(0, 0) // the next phase starts without a bar of its own
	return dir, err
}

// hubModel lists a Hugging Face repository's files at the pinned revision (keep: the ones the backend needs).
func hubModel(ctx context.Context, store *models.Store, m *Model, keep func(string) bool) (models.Model, error) {
	hm, err := store.FromHub(ctx, m.Name, m.repo, m.rev, keep)
	if errors.Is(err, models.ErrGated) {
		return hm, GatedError(m.repo)
	}
	return hm, err
}
