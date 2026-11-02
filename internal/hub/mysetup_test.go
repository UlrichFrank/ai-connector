package hub

import (
	"strings"
	"testing"
)

// A token sets models up on its person's workers only: the list, a download (followed, canceled, done — then the
// model is offered), and nothing on another person's worker.
func TestMySetup(t *testing.T) {
	setupWorld(t)
	s, srv := newHub(t, t.TempDir())
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	bob := signInAs(t, base, "Bob's phone", "Bob")
	aliceID, wtok := enroll(t, base, alice, "Alice's Mac")
	mac := startSetupMac(t, base, wtok)
	_, aliceApp := makeClient(t, base, alice, "alice-app")
	_, bobApp := makeClient(t, base, bob, "bob-app")
	waitFor(t, "the Mac known", func() bool { return workerItems(t, base, alice)[aliceID].Specs != nil })

	var setup struct {
		User    string
		Workers []struct {
			ID   string
			Free int64
		}
		Models []struct {
			ID          string
			Recommended bool
			Here        bool
			Worker      idName
		}
	}
	decode(t, do(t, "GET", base+"/api/my/setup", aliceApp, nil), &setup)
	if setup.User != "Alice" || len(setup.Workers) != 1 || setup.Workers[0].ID != aliceID || len(setup.Models) != 5 {
		t.Fatalf("Alice's setup %+v", setup)
	}
	rec := map[string]bool{}
	for _, m := range setup.Models {
		rec[m.ID] = m.Recommended
		if m.Worker.ID != aliceID || m.Here {
			t.Fatalf("model %+v", m)
		}
	}
	if !rec["qwen3.6-35b-a3b"] || rec["qwen3.5-4b"] {
		t.Fatalf("recommended %v", rec)
	}

	// Bob has no worker: nothing listed, nothing downloaded (not onto Alice's)
	decode(t, do(t, "GET", base+"/api/my/setup", bobApp, nil), &setup)
	if setup.User != "Bob" || len(setup.Workers) != 0 || len(setup.Models) != 0 {
		t.Fatalf("Bob's setup %+v", setup)
	}
	if code, b := post(t, base+"/api/my/downloads", bobApp, `{"model":"qwen3.5-4b"}`); code != 409 {
		t.Fatalf("Bob downloads: %d %s", code, b)
	}
	if code, b := post(t, base+"/api/my/downloads", bobApp, `{"model":"qwen3.5-4b","worker":"`+aliceID+`"}`); code != 409 {
		t.Fatalf("Bob downloads to Alice's: %d %s", code, b)
	}
	// a token without an owner sets nothing up
	oldTok, _, err := s.clients.add("old-app", "")
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, "GET", base+"/api/my/setup", oldTok, nil); r.StatusCode != 403 {
		t.Fatalf("no owner: %d", r.StatusCode)
	}

	// a download: started, followed, canceled
	var d myDownload
	code, b := post(t, base+"/api/my/downloads", aliceApp, `{"model":"qwen3.5-4b"}`)
	if code != 200 {
		t.Fatalf("start %d %s", code, b)
	}
	follow := func() myDownload {
		var d myDownload
		decode(t, do(t, "GET", base+"/api/my/downloads?model=qwen3.5-4b", aliceApp, nil), &d)
		return d
	}
	waitFor(t, "the download downloading", func() bool { d = follow(); return d.State == "downloading" && d.Bytes > 0 })
	if d.Worker.ID != aliceID {
		t.Fatalf("download %+v", d)
	}
	if r := do(t, "DELETE", base+"/api/my/downloads?model=qwen3.5-4b", bobApp, nil); r.StatusCode != 404 {
		t.Fatalf("Bob cancels Alice's download: %d", r.StatusCode)
	}
	if r := do(t, "DELETE", base+"/api/my/downloads?model=qwen3.5-4b", aliceApp, nil); r.StatusCode != 204 {
		t.Fatalf("cancel: %d", r.StatusCode)
	}
	waitFor(t, "the download canceled", func() bool { return follow().State == "canceled" })

	// again, to the end: the model is offered then
	if code, b := post(t, base+"/api/my/downloads", aliceApp, `{"model":"qwen3.5-4b","worker":"Alice's Mac"}`); code != 200 {
		t.Fatalf("start again %d %s", code, b)
	}
	mac.release <- struct{}{}
	waitFor(t, "the download done", func() bool { return follow().State == "done" })
	waitFor(t, "the model offered", func() bool {
		var c capabilities
		decode(t, do(t, "GET", base+"/api/capabilities", aliceApp, nil), &c)
		for _, m := range c.Models {
			if m.ID == "qwen3.5-4b" {
				return true
			}
		}
		return false
	})
	var all []myDownload
	decode(t, do(t, "GET", base+"/api/my/downloads", aliceApp, nil), &all)
	if len(all) == 0 || !strings.Contains(all[0].Model, "qwen3.5-4b") {
		t.Fatalf("all downloads %+v", all)
	}
}
