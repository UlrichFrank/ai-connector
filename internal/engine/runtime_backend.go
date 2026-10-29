package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

// RuntimeBackend runs the models of a generic Python runtime (RuntimeManifest.Generic): no Go of its own. The worker
// script gets the job protocol's request — the model's defaults with the request's fields over them, the model's
// files (path, or dirs by source key), its params (checked, defaults filled in) and where to write the result — and
// answers with the usual events.
type RuntimeBackend struct {
	Root    string
	Store   *models.Store
	Runtime string

	mu    sync.Mutex
	w     *pyWorker
	state status
}

func (b *RuntimeBackend) Name() string   { return b.Runtime }
func (b *RuntimeBackend) Heavy() bool    { return runtimeManifest(b.Runtime).heavy() }
func (b *RuntimeBackend) Loaded() string { return b.state.get() }

func (b *RuntimeBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.w != nil {
		b.w.stop()
	}
	b.state.set("")
}

func (b *RuntimeBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rt := runtimeManifest(b.Runtime)
	params, err := CheckParams(m, r.Params)
	if err != nil {
		return err
	}
	req, err := mergedRequest(m.Defaults, r)
	if err != nil {
		return err
	}
	req["model"] = orStr(m.workerModel, m.ID)
	req["params"] = params
	if m.repo != "" {
		var keep func(string) bool
		switch {
		case len(m.files) > 0:
			keep = func(p string) bool { return models.Matches(p, m.files...) }
		case len(m.ignore) > 0:
			keep = func(p string) bool { return !models.Matches(p, m.ignore...) }
		}
		hm, err := hubModel(ctx, b.Store, m, keep)
		if err != nil {
			return err
		}
		dir, err := fetch(ctx, b.Store, hm, emit)
		if err != nil {
			return err
		}
		req["path"] = dir
		m.Here, m.Where = true, dir
	}
	if len(m.repos) > 0 {
		dirs := map[string]string{}
		for _, hr := range m.repos {
			hm, err := b.Store.FromHub(ctx, m.Name, hr.repo, hr.rev, hr.keep())
			if errors.Is(err, models.ErrGated) {
				return GatedError(hr.repo)
			}
			if err != nil {
				return err
			}
			if dirs[hr.key], err = fetch(ctx, b.Store, hm, emit); err != nil {
				return err
			}
		}
		req["dirs"] = dirs
	}
	if b.w == nil {
		b.w = &pyWorker{env: runtimeEnv(b.Runtime), script: rt.Script, src: rt.source}
	}
	if err := b.w.start(ctx, b.Root, emit); err != nil {
		return err
	}
	req["output"] = filepath.Join(outDir, fmt.Sprintf("%s-%s.%s", time.Now().Format("20060102-150405"), m.ID, rt.Output))
	b.state.set(m.ID)
	ev, err := b.w.call(ctx, req, emit)
	if err != nil {
		if !b.w.running() {
			b.state.set("")
		}
		return err
	}
	emit(Event{Type: "file", File: ev.File})
	st := map[string]any{"seconds": ev.Seconds, "peakGB": ev.PeakGB}
	if ev.Width > 0 && ev.Height > 0 {
		st["width"], st["height"] = ev.Width, ev.Height
	}
	emit(Event{Type: "stats", Data: st})
	return nil
}

// mergedRequest is the request as the worker script gets it: the model's defaults, the request's fields over them.
func mergedRequest(defaults, r Request) (map[string]any, error) {
	out := map[string]any{}
	for _, v := range []Request{defaults, r} {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var f map[string]any
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		for k, x := range f {
			if x != nil && x != "" {
				out[k] = x
			}
		}
	}
	delete(out, "params")
	return out, nil
}

// ParamSpec describes one of a model's params (a small part of JSON Schema).
type ParamSpec struct {
	Type        string   `json:"type"` // integer, number, string, boolean
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Minimum     *float64 `json:"minimum,omitempty"`
	Maximum     *float64 `json:"maximum,omitempty"`
	Enum        []any    `json:"enum,omitempty"`
}

func (p ParamSpec) check() error {
	if !slices.Contains([]string{"integer", "number", "string", "boolean"}, p.Type) {
		return fmt.Errorf("type %q is none of integer, number, string, boolean", p.Type)
	}
	if p.Default != nil {
		if _, err := p.value(p.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// value checks v against the spec and returns it as the type says.
func (p ParamSpec) value(v any) (any, error) {
	var out any
	switch p.Type {
	case "integer", "number":
		f, ok := v.(float64)
		if !ok {
			if i, isInt := v.(int); isInt {
				f, ok = float64(i), true
			}
		}
		if !ok {
			return nil, fmt.Errorf("%v is no number", v)
		}
		if p.Type == "integer" && f != math.Trunc(f) {
			return nil, fmt.Errorf("%v is no whole number", v)
		}
		if p.Minimum != nil && f < *p.Minimum {
			return nil, fmt.Errorf("%v is below %v", v, *p.Minimum)
		}
		if p.Maximum != nil && f > *p.Maximum {
			return nil, fmt.Errorf("%v is above %v", v, *p.Maximum)
		}
		out = f
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%v is no string", v)
		}
		out = s
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%v is not true or false", v)
		}
		out = b
	}
	if len(p.Enum) > 0 && !slices.ContainsFunc(p.Enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(out) }) {
		return nil, fmt.Errorf("%v is none of %v", v, p.Enum)
	}
	return out, nil
}

// CheckParams checks a request's params against the model's (an unknown one, a wrong type, out of range: an error
// naming it) and fills in the defaults.
func CheckParams(m *Model, given map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var names []string
	for n := range given {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		spec, ok := m.Params[n]
		if !ok {
			known := make([]string, 0, len(m.Params))
			for k := range m.Params {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("%s takes no param %q (it takes: %s)", m.ID, n, strings.Join(known, ", "))
		}
		v, err := spec.value(given[n])
		if err != nil {
			return nil, fmt.Errorf("%s: param %s: %w", m.ID, n, err)
		}
		out[n] = v
	}
	for n, spec := range m.Params {
		if _, ok := out[n]; !ok && spec.Default != nil {
			out[n], _ = spec.value(spec.Default)
		}
	}
	return out, nil
}
