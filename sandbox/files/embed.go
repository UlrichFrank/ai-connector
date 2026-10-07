// Package sandboxfiles holds the build context of the files sandbox image (Containerfile and the in-container
// scripts), embedded so the mlxlab binary can build the image itself (internal/sandbox/files).
package sandboxfiles

import "embed"

// Context is the image's build context.
//
//go:embed Containerfile runner.py markdown_pdf.py
var Context embed.FS
