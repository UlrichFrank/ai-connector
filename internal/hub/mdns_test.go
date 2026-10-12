package hub

import (
	"slices"
	"testing"
)

func TestMDNSTXT(t *testing.T) {
	if got := mdnsTXT(""); !slices.Equal(got, []string{"role=hub", "path=/"}) {
		t.Fatalf("no public URL: %q", got)
	}
	if got := mdnsTXT("https://ai.example.org"); !slices.Equal(got, []string{"url=https://ai.example.org", "role=hub", "path=/"}) {
		t.Fatalf("public URL: %q", got)
	}
}

func TestMDNSFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": true, "1": true, "true": true, "on": true, "0": false, "false": false, " OFF ": false} {
		t.Setenv("MLXLAB_MDNS", v)
		if got := FromEnv().MDNS; got != want {
			t.Fatalf("MLXLAB_MDNS=%q: %v", v, got)
		}
	}
	t.Setenv("MLXLAB_MDNS_NAME", " Hub ")
	t.Setenv("MLXLAB_PUBLIC_URL", "https://ai.example.org")
	if c := FromEnv(); c.MDNSName != "Hub" || c.PublicURL != "https://ai.example.org" {
		t.Fatalf("config %+v", c)
	}
}
