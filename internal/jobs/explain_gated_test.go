package jobs

import (
	"strings"
	"testing"
)

func TestExplainGatedFetch(t *testing.T) {
	p := Explain("model_index.json of black-forest-labs/FLUX.2-klein-9B could not be fetched (the local Hugging Face cache: not found; huggingface.co: HTTP 403)")
	if p.Title != "The model needs your consent on Hugging Face" || !strings.Contains(p.Hint, "https://huggingface.co/black-forest-labs/FLUX.2-klein-9B") {
		t.Fatalf("%+v", p)
	}
	// a network failure stays a download failure
	if p := Explain("model.safetensors of a/b could not be fetched (huggingface.co: i/o timeout)"); p.Title != "A download failed" {
		t.Fatalf("%+v", p)
	}
}
