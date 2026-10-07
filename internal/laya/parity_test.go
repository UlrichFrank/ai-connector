package laya

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// TestParity compares with the Python package on the real checkpoint — when it is there: MLXLAB_LAYA=<checkpoint
// dir, e.g. …/typed-decisions> and MLXLAB_REF=<models.json written by laya.predict_batch on the CPU>.
func TestParity(t *testing.T) {
	dir, ref := os.Getenv("MLXLAB_LAYA"), os.Getenv("MLXLAB_REF")
	if dir == "" || ref == "" {
		t.Skip("MLXLAB_LAYA / MLXLAB_REF not set")
	}
	raw, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Classify []struct {
			Instructions string          `json:"instructions"`
			Options      json.RawMessage `json:"options"`
			States       []string        `json:"states"`
			Answers      []Answer        `json:"answers"`
		} `json:"classify"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded in %v", time.Since(start))
	for _, c := range r.Classify {
		opts, err := orderedOptions(c.Options)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		got, err := m.Choose(c.States, c.Instructions, opts, 32)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d states in %v", len(c.States), time.Since(start))
		for i, want := range c.Answers {
			g := got[i]
			if g.Choice != want.Choice {
				t.Errorf("%q / %s: choice %q, want %q (%v vs %v)", c.Instructions, c.States[i][:min(40, len(c.States[i]))], g.Choice, want.Choice, g.Probabilities, want.Probabilities)
			}
			for k, p := range want.Probabilities {
				if math.Abs(g.Probabilities[k]-p) > 2e-3 {
					t.Errorf("%q / %s: P(%s) = %v, want %v", c.Instructions, c.States[i][:min(40, len(c.States[i]))], k, g.Probabilities[k], p)
				}
			}
		}
	}
}

// orderedOptions reads a JSON object into options in the object's order.
func orderedOptions(raw json.RawMessage) ([]Option, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out []Option
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, Option{Label: k.(string), Description: v})
	}
	return out, nil
}
