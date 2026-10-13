package hub

import (
	"slices"
	"strings"
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

func TestAvahiService(t *testing.T) {
	got := string(AvahiService("", "https://ai.example.org/?a=1&b=2", 443))
	for _, want := range []string{
		"<name>AI Connector</name>",
		"<type>_ai-connector._tcp</type>",
		"<port>443</port>",
		"<txt-record>url=https://ai.example.org/?a=1&amp;b=2</txt-record>",
		"<txt-record>role=hub</txt-record>",
		"<txt-record>path=/</txt-record>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if got := string(AvahiService("Küche <1>", "", 8080)); !strings.Contains(got, "<name>Küche &lt;1&gt;</name>") || strings.Contains(got, "url=") {
		t.Fatalf("name/no url:\n%s", got)
	}
}
