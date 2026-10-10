package hub

import (
	"bytes"
	"encoding/binary"
)

// pngMeta are the chunks pngWithoutMeta drops: text (mflux writes the prompt and its settings there as XMP and
// IPTC), EXIF and the physical pixel size. WebKit reads a density from them (144 dpi → a 2048 px picture whose
// naturalWidth is 1024), and none of them is needed to show the picture.
var pngMeta = map[string]bool{"tEXt": true, "zTXt": true, "iTXt": true, "eXIf": true, "pHYs": true}

// pngWithoutMeta is the PNG without its metadata chunks; anything that is not a well-formed PNG comes back as it was.
func pngWithoutMeta(b []byte) []byte {
	sig := []byte("\x89PNG\r\n\x1a\n")
	if !bytes.HasPrefix(b, sig) {
		return b
	}
	out := append(make([]byte, 0, len(b)), sig...)
	for i := len(sig); i < len(b); {
		if i+12 > len(b) {
			return b
		}
		n := int(binary.BigEndian.Uint32(b[i:]))
		end := i + 12 + n
		if n < 0 || end > len(b) || end < i {
			return b
		}
		if !pngMeta[string(b[i+4:i+8])] {
			out = append(out, b[i:end]...)
		}
		i = end
	}
	return out
}
