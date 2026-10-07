//go:build communitylive

package engine

// A real song with the curated deep house community style on ACE-Step 1.5 turbo, against the same seed without it
// (and with only its trigger word, no LoRA): heavy (GPU, the LoRA downloads), so it runs only when asked for:
//
//	MLXLAB_LIVE_HOME=<a scratch home with models/ and envs/> MLXLAB_LIVE_OUT=<dir> \
//	  go test -tags communitylive -run TestCommunityStyleLive -timeout 30m -v ./internal/engine/
//
// The files land in MLXLAB_LIVE_OUT (with-style.wav, trigger-only.wav, without-style.wav).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestCommunityStyleLive(t *testing.T) {
	home, out := os.Getenv("MLXLAB_LIVE_HOME"), os.Getenv("MLXLAB_LIVE_OUT")
	if home == "" || out == "" {
		t.Skip("MLXLAB_LIVE_HOME and MLXLAB_LIVE_OUT")
	}
	store := &models.Store{Dir: filepath.Join(home, "models"), Sources: []models.Source{models.HFCache(), models.HuggingFace()}}
	b := &MusicBackend{Root: home, Store: store, Runtimes: MusicRuntimes()}
	defer b.Unload()
	var m *Model
	for _, x := range musicModels() {
		if x.ID == "ace-step-1.5-turbo" {
			m = x
		}
	}
	ctx := context.Background()
	st, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: CommunityStyles[0].Repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("style %s: %s@%s trigger %q base %s weights %s recommended %+v", st.ID, st.Source.Repo, st.Source.Rev, st.Trigger, st.BaseModel,
		st.WeightsFile, *st.Recommended)
	prompt := st.Examples[0].Prompt
	lock := false
	runs := []struct {
		name string
		r    Request
	}{
		{"with-style", Request{Prompt: prompt, Style: st.ID}},
		{"trigger-only", Request{Prompt: st.Trigger + ", " + prompt}},
		{"without-style", Request{Prompt: prompt}},
	}
	for _, run := range runs {
		r := run.r
		r.Model, r.Duration, r.BPM, r.Seed, r.LockTempo = m.ID, 30, 122, 4242, &lock
		tmp := filepath.Join(out, "tmp-"+run.name)
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			t.Fatal(err)
		}
		var file string
		emit := func(e Event) {
			switch e.Type {
			case "file":
				if file == "" {
					file = e.File
				}
			case "stats":
				t.Logf("%s stats %v", run.name, e.Data)
			case "log":
				if strings.Contains(e.Text, "LoRA") || strings.Contains(e.Text, "recommended") || strings.Contains(e.Text, "style") {
					t.Logf("%s log: %s", run.name, e.Text)
				}
			}
		}
		t0 := time.Now()
		if err := b.Run(ctx, m, r, tmp, emit); err != nil {
			t.Fatalf("%s: %v", run.name, err)
		}
		if err := os.Rename(file, filepath.Join(out, run.name+".wav")); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %.1f s", run.name, time.Since(t0).Seconds())
	}
}
