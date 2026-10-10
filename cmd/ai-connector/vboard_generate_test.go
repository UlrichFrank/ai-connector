package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

// --generators is kept like --agents; an empty flag clears it.
func TestResolveVboardGenerators(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	none := func(string) string { return "" }
	if _, err := resolveVboard(vboardSettings{URL: "https://v", Token: "vbp_1", Generators: "flux2-klein-4b, ltx-2.5-q4"}, nil, none, file); err != nil {
		t.Fatal(err)
	}
	s, _ := resolveVboard(vboardSettings{}, nil, none, file)
	if got := splitList(s.Generators); !reflect.DeepEqual(got, []string{"flux2-klein-4b", "ltx-2.5-q4"}) {
		t.Fatalf("kept %q → %q", s.Generators, got)
	}
	if s, _ := resolveVboard(vboardSettings{}, map[string]bool{"generators": true}, none, file); s.Generators != "" || splitList(s.Generators) != nil {
		t.Fatalf("--generators=: want none, got %+v", s)
	}
}

func TestVboardGeneratorsDefault(t *testing.T) {
	for in, want := range map[string][]string{"": {"*"}, "none": nil, "flux2-klein-4b, *": {"flux2-klein-4b", "*"}} {
		if got := vboardGenerators(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
