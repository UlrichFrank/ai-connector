package jobs

import (
	"regexp"
	"strings"
)

// Problem is a failure in words: what happened and what to do; the full text stays in Job.Error.
type Problem struct {
	Title string `json:"title"`
	Hint  string `json:"hint,omitempty"`
}

var problems = []struct {
	match *regexp.Regexp
	p     Problem
}{
	{regexp.MustCompile(`(?i)AGX_RELAX_CDM_CTXSTORE_TIMEOUT|ImpactingInteractivity|GPU Timeout|GPU watchdog|its watchdog`),
		Problem{"macOS stopped the GPU job", "It held the GPU too long while the display was on (the GPU watchdog). Retry — a movie keeps its finished clips, and a stopped clip is tried again block by block (low RAM) — or let the display sleep while it renders, or choose a smaller size or fewer frames."}},
	{regexp.MustCompile(`(?i)out of memory|OutOfMemory|Insufficient Memory|failed to allocate|MTLHeap|Cannot allocate`),
		Problem{"Not enough memory", "Free the memory of other models (Unload) and retry, or choose a smaller size, fewer frames or a quantised model."}},
	{regexp.MustCompile(`(?i)is gated on Hugging Face|GatedRepoError|Cannot access gated repo`),
		Problem{"The model needs your consent on Hugging Face", "Accept its licence on its Hugging Face page, log in on the Mac (uvx hf auth login), then retry."}},
	{regexp.MustCompile(`(?i)could not be fetched|no such host|connection reset|timeout awaiting|TLS handshake|i/o timeout`),
		Problem{"A download failed", "Check the Mac's network and retry — downloads continue where they stopped."}},
	{regexp.MustCompile(`(?i)checksum mismatch`),
		Problem{"A downloaded file was damaged", "Retry: the damaged file is fetched again."}},
	{regexp.MustCompile(`(?i)uv is needed|uv venv|uv pip install|fetching uv`),
		Problem{"The model's runtime could not be installed", "Check that the Mac is online (uv is fetched by itself when it is not installed), then retry."}},
	{regexp.MustCompile(`(?i)ffmpeg`),
		Problem{"ffmpeg is missing or failed", "Install it with brew install ffmpeg, then retry."}},
	{regexp.MustCompile(`(?i)the worker stopped|signal: killed|exit status`),
		Problem{"The model stopped unexpectedly", "Retry; if it keeps failing, the details below say why."}},
}

// Explain turns an error into a Problem (a generic one when it is not a known kind).
// gatedFetch: the model store's "<file> of <repo> could not be fetched (… huggingface.co: HTTP 403)" — the token works
// but this repository's licence was not accepted (each gated repository has its own: FLUX.2 klein 9B apart from 4B).
var gatedFetch = regexp.MustCompile(`(?i)of ([\w.-]+/[\w.-]+) could not be fetched.*huggingface\.co: HTTP 40[13]`)

func Explain(err string) Problem {
	if m := gatedFetch.FindStringSubmatch(err); m != nil {
		return Problem{"The model needs your consent on Hugging Face",
			"Open https://huggingface.co/" + m[1] + ", sign in with the account of the Mac's Hugging Face token and accept the licence " +
				"(every gated model has its own), or log in on the Mac first (uvx hf auth login). Then retry."}
	}
	for _, p := range problems {
		if p.match.MatchString(err) {
			return p.p
		}
	}
	first := strings.TrimSpace(strings.SplitN(err, "\n", 2)[0])
	if len(first) > 140 {
		first = first[:140] + "…"
	}
	return Problem{Title: first}
}
