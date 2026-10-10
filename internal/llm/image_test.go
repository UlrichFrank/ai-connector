package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func describe(parts []Part) string {
	var s []string
	for _, p := range parts {
		if p.Image != nil {
			s = append(s, "<"+p.Image.ID+">")
		} else {
			s = append(s, p.Text)
		}
	}
	return strings.Join(s, "|")
}

// The images go where their tags are (the tag stays in the text), in the text's order; an image without a tag
// follows at the end.
func TestParts(t *testing.T) {
	ims := []Image{{ID: "image-1"}, {ID: "image-2"}, {ID: "image-10"}}
	for _, x := range []struct {
		text string
		ims  []Image
		want string
	}{
		{"what is [image-1]?", ims[:1], "what is [image-1]|<image-1>|?"},
		{"[image-2] before [image-1]", ims[:2], "[image-2]|<image-2>| before [image-1]|<image-1>"},
		{"[image-10] not [image-1]", []Image{ims[0], ims[2]}, "[image-10]|<image-10>| not [image-1]|<image-1>"},
		{"no tag", ims[:1], "no tag |[image-1]|<image-1>"},
		{"", ims[:1], "[image-1]|<image-1>"},
		{"plain", nil, "plain"},
	} {
		if got := describe(Parts(x.text, x.ims)); got != x.want {
			t.Errorf("%q: %q, want %q", x.text, got, x.want)
		}
	}
}

func TestImageChecksAndJSON(t *testing.T) {
	mime, data, err := ParseDataURL("data:image/png;base64,iVBORw==")
	if err != nil || mime != "image/png" || len(data) != 4 {
		t.Fatalf("%s %v %v", mime, data, err)
	}
	for _, bad := range []string{"https://x/a.png", "data:image/bmp;base64,AA==", "data:image/png,raw", "data:image/png;base64,!!"} {
		if _, _, err := ParseDataURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, im := range []Image{{ID: "a b", MIME: "image/png", Data: []byte{1}}, {ID: "i", MIME: "text/plain", Data: []byte{1}},
		{ID: "i", MIME: "image/png"}, {ID: "i", MIME: "image/png", Data: make([]byte, MaxImageBytes+1)}} {
		if CheckImage(im) == nil {
			t.Errorf("%+v accepted", im.ID)
		}
	}
	// JSON: images[{id, mime, data (base64)}] next to the message's fields
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"x [image-1]","images":[{"id":"image-1","mime":"image/webp","data":"AQID"}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Images) != 1 || string(m.Images[0].Data) != "\x01\x02\x03" || !HasImages([]Message{m}) {
		t.Fatalf("%+v", m)
	}
	if got := OmitImages(m.Content, m.Images); got != "x (image omitted)" {
		t.Fatal(got)
	}
}
