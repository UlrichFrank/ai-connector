package hub

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"testing"
)

// The memory the router reckons each catalogue model needs (needGB), pinned: when the peaks move out of route.go
// (into the models' descriptions, which the workers report), every model must still need what it needs today. The
// models are the worker catalogue's golden file (read as a file: the server does not import the engine).
// MLXLAB_UPDATE_GOLDEN=1 writes testdata/need.golden.json anew.

const goldenNeed = "testdata/need.golden.json"

func TestNeedGolden(t *testing.T) {
	b, err := os.ReadFile("../engine/testdata/catalog.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Models []struct {
			API catalogModel `json:"api"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	need := map[string]string{}
	var ids []string
	for _, m := range cat.Models {
		need[m.API.ID] = strconv.FormatFloat(needGB(m.API), 'f', 2, 64)
		ids = append(ids, m.API.ID)
	}
	if len(ids) == 0 {
		t.Fatal("no models in the catalogue's golden file")
	}
	sort.Strings(ids)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, id := range ids {
		k, _ := json.Marshal(id)
		buf.WriteString("  " + string(k) + ": " + need[id])
		if i < len(ids)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	got := buf.Bytes()
	if os.Getenv("MLXLAB_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenNeed, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenNeed)
	if err != nil {
		t.Fatalf("%v (MLXLAB_UPDATE_GOLDEN=1 writes it)", err)
	}
	var w map[string]float64
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("the golden file does not parse: %v", err)
	}
	for _, id := range ids {
		was, ok := w[id]
		switch {
		case !ok:
			t.Errorf("%s: new, needs %s GB", id, need[id])
		case strconv.FormatFloat(was, 'f', 2, 64) != need[id]:
			t.Errorf("%s: needed %.2f GB, now %s GB", id, was, need[id])
		}
		delete(w, id)
	}
	for id := range w {
		t.Errorf("%s: gone from the catalogue", id)
	}
	if !t.Failed() && !bytes.Equal(got, want) {
		t.Errorf("%s differs (formatting?); MLXLAB_UPDATE_GOLDEN=1 writes it", goldenNeed)
	}
}
