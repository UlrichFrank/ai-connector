package hub

import "testing"

func TestPlatform(t *testing.T) {
	for in, want := range map[string]string{
		"Darwin-arm64": "darwin-arm64", "Linux-x86_64": "linux-amd64", "Linux-aarch64": "linux-arm64",
		"linux-arm64": "linux-arm64", "Windows-AMD64": "windows-amd64", "windows-amd64": "windows-amd64",
		"Darwin-x86_64": "", "Windows-ARM64": "", "Linux": "", "": "",
	} {
		if got := platform(in); got != want {
			t.Errorf("platform(%q) = %q, want %q", in, got, want)
		}
	}
}
