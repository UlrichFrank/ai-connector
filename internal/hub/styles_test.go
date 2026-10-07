package hub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/styles"
)

// Styles stay on the Mac that trained them: the hub lists every Mac's styles (each with its Mac), sends what is
// addressed to a style to its Mac, and a job with a style runs there.
func TestStylesStayOnTheirMac(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	miniID, miniTok := enroll(t, base, device, "Mac mini")
	studioID, studioTok := enroll(t, base, device, "Mac Studio")
	mini := startMac(t, base, miniTok, 16)
	studio := startMac(t, base, studioTok, 64)
	waitFor(t, "both Macs known", func() bool {
		ws := workerItems(t, base, device)
		return ws[miniID].Specs != nil && ws[studioID].Specs != nil
	})

	// a style on the mini (not the primary)
	st, err := styles.Open(filepath.Join(mini.mgr.Dir, "styles")).Create("Night Drive", "small", styles.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Styles []struct {
			ID     string            `json:"id"`
			Name   string            `json:"name"`
			Worker map[string]string `json:"worker"`
		} `json:"styles"`
		Community []map[string]any `json:"community"`
	}
	decode(t, do(t, "GET", base+"/api/styles", device, nil), &list)
	if len(list.Styles) != 1 || list.Styles[0].ID != st.ID || list.Styles[0].Worker["id"] != miniID || len(list.Community) == 0 {
		t.Fatalf("styles %+v", list)
	}
	// the curated community LoRAs, from the Mac the user picks
	var cs struct {
		Community []map[string]any `json:"community"`
	}
	decode(t, do(t, "GET", base+"/api/community-styles?worker="+miniID, device, nil), &cs)
	if len(cs.Community) == 0 {
		t.Fatalf("community %+v", cs)
	}

	// a rename goes to the mini
	r := do(t, "PATCH", base+"/api/styles/"+st.ID, device, strings.NewReader(`{"name":"Night Drive 2"}`))
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("patch: %d", r.StatusCode)
	}
	if got, _ := styles.Open(filepath.Join(mini.mgr.Dir, "styles")).Get(st.ID); got.Name != "Night Drive 2" {
		t.Fatalf("renamed: %+v", got)
	}
	// unknown: 404 from the hub
	if r := do(t, "GET", base+"/api/styles/nope-123", device, nil); r.StatusCode != 404 {
		t.Fatalf("unknown style: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// a song with the style runs on the mini, although the studio would be chosen otherwise
	code, j, e := submitJob(t, base, device, `{"model":"small","prompt":"x","style":"`+st.ID+`"}`)
	if code != 200 || j.Worker["id"] != miniID {
		t.Fatalf("job: %d %+v %v", code, j, e)
	}
	waitDone(t, base, device, j.ID)
	code, _, e = submitJob(t, base, device, `{"model":"small","prompt":"x","style":"gone-1"}`)
	if code != 404 {
		t.Fatalf("job with an unknown style: %d %v", code, e)
	}
	_ = studio
}
