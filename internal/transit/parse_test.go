package transit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/transit/transittest"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(transittest.Fixture(name), v); err != nil {
		t.Fatal(name, err)
	}
}

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, Berlin)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDBStops(t *testing.T) {
	var locs []dbLocation
	load(t, "db_orte.json", &locs)
	stops := parseDBLocations(locs, 5)
	if len(stops) == 0 || stops[0].ID != "db:8004070" || stops[0].Name != "Mössingen" || stops[0].Provider != "db" {
		t.Fatalf("stops = %+v", stops)
	}
	if !wants(stops[0].Products, Train) || !wants(stops[0].Products, Bus) || stops[0].Lat < 48 {
		t.Errorf("first stop = %+v", stops[0])
	}
}

func TestDBDepartures(t *testing.T) {
	var res struct {
		Entries []dbAbfahrt `json:"entries"`
	}
	load(t, "db_abfahrten.json", &res)
	all := parseDBDepartures(res.Entries)
	if len(all) != len(res.Entries) {
		t.Fatalf("%d of %d departures parsed", len(all), len(res.Entries))
	}
	mex := all[0]
	if mex.Line != "MEX18" || mex.Product != Train || mex.Direction != "Stuttgart Hbf" || mex.Platform != "3" ||
		mex.Delay == nil || *mex.Delay != 70 || !strings.HasPrefix(mex.TripID, "db:2|#VN#") {
		t.Errorf("MEX18 = %+v", mex)
	}
	if mex.Planned.Format(time.RFC3339) != "2026-10-04T06:33:00+02:00" {
		t.Errorf("planned = %v", mex.Planned)
	}
	if len(mex.Remarks) != 1 || mex.Remarks[0] != "Verspätete Bereitstellung des Zuges" {
		t.Errorf("remarks (low priority ones must be left out) = %q", mex.Remarks)
	}
	// the window: from 07:10, 20 min, trains only; the MEX18 planned 06:33 leaves at 07:43 — outside
	q := DepartureQuery{When: at("2026-10-04 07:10"), Duration: 20 * time.Minute, Products: []Product{Train}}
	win := windowDepartures(all, q)
	var lines []string
	for _, d := range win {
		lines = append(lines, d.Line+"@"+effective(d).Format("15:04"))
	}
	if got := strings.Join(lines, " "); got != "RE6a@07:10 RB63@07:11 RB74@07:23 RB66@07:29" {
		t.Errorf("window = %s", got)
	}
	q.Duration = time.Hour
	q.Limit = 3
	win = windowDepartures(all, q)
	if len(win) != 3 {
		t.Errorf("limit: %d", len(win))
	}
	for _, d := range all {
		if d.Product == Bus && !strings.HasPrefix(d.Line, "Bus ") && d.Line != "" {
			t.Errorf("bus line %q", d.Line)
		}
	}
}

func TestDBCancelled(t *testing.T) {
	var es []dbAbfahrt
	if err := json.Unmarshal([]byte(`[{"zeit":"2026-10-04T07:29:00","gleis":"12","terminus":"Sigmaringen",
		"verkehrmittel":{"mittelText":"RB66","produktGattung":"REGIONAL"},
		"meldungen":[{"prioritaet":"HOCH","text":"Halt entfällt","type":"HALT_AUSFALL"}]},
		{"zeit":"2026-10-04T07:30:00","ezGleis":"4","gleis":"5","terminus":"X","canceled":true,
		"verkehrmittel":{"mittelText":"RE6","produktGattung":"REGIONAL"}}]`), &es); err != nil {
		t.Fatal(err)
	}
	ds := parseDBDepartures(es)
	if !ds[0].Cancelled || !ds[1].Cancelled {
		t.Errorf("cancelled = %v %v", ds[0].Cancelled, ds[1].Cancelled)
	}
	if ds[1].Platform != "4" || ds[1].PlannedPlatform != "5" {
		t.Errorf("platform change = %q from %q", ds[1].Platform, ds[1].PlannedPlatform)
	}
	text := (&DeparturesResult{Stop: Stop{Name: "T", ID: "db:1"}, When: at("2026-10-04 07:10"), Minutes: 60,
		Departures: ds}).Text()
	if !strings.Contains(text, "07:29 · CANCELLED · RB66 → Sigmaringen · platform 12") ||
		!strings.Contains(text, "platform 4 (changed from 5)") {
		t.Errorf("text:\n%s", text)
	}
}

func TestDBJourneys(t *testing.T) {
	var res struct {
		Verbindungen []dbVerbindung `json:"verbindungen"`
	}
	load(t, "db_fahrplan.json", &res)
	js := parseDBJourneys(res.Verbindungen, true)
	if len(js) == 0 {
		t.Fatal("no journeys")
	}
	j := js[0]
	if len(j.Legs) != 2 || j.Transfers != 1 || j.Legs[0].Line != "RB63" || j.Legs[1].Line != "S1" {
		t.Fatalf("journey 1 = %+v", j)
	}
	l := j.Legs[0]
	if l.DepPlatform != "13" || l.DepDelay == nil || *l.DepDelay != 0 || l.Dep.Format("15:04") != "07:11" ||
		len(l.Stops) < 2 || l.Stops[0].Name != "Tübingen Hbf" {
		t.Errorf("leg 1 = %+v", l)
	}
	if j.Legs[1].DepDelay != nil {
		t.Errorf("no real-time data must be nil, not %v", *j.Legs[1].DepDelay)
	}
	late := js[2]
	if d := late.Legs[0].DepDelay; d == nil || *d != 70 || late.Duration != 60 {
		t.Errorf("late journey = %+v (duration %d)", late.Legs[0], late.Duration)
	}

	load(t, "db_fahrplan_tomorrow.json", &res)
	js = parseDBJourneys(res.Verbindungen, false)
	var walks, buses int
	for _, j := range js {
		for _, l := range j.Legs {
			if l.Walking {
				walks++
				if l.Product != Walk || l.Line != "" {
					t.Errorf("walk leg = %+v", l)
				}
			}
			if l.Product == Bus {
				buses++
			}
			if l.Stops != nil {
				t.Error("stops without withStops")
			}
		}
	}
	if walks == 0 || buses == 0 {
		t.Errorf("tomorrow: %d walks, %d bus legs", walks, buses)
	}
}

func TestDBTrip(t *testing.T) {
	var f dbFahrt
	load(t, "db_fahrt.json", &f)
	tr := parseDBTrip("2|#VN#1#x", f)
	if tr.ID != "db:2|#VN#1#x" || len(tr.Stops) != 11 || tr.Stops[0].Name != "Tübingen Hbf" ||
		tr.Direction != "Stuttgart Hbf" || !strings.HasPrefix(tr.Line, "RE ") {
		t.Errorf("trip = %+v", tr)
	}
	if tr.Stops[0].Dep == nil || tr.Stops[0].Arr != nil {
		t.Errorf("first stop = %+v", tr.Stops[0])
	}
	text := (&TripResult{Trip: tr}).Text()
	if !strings.Contains(text, "Tübingen Hbf (pl. 3)") {
		t.Errorf("text:\n%s", text)
	}
}

func TestEFAStops(t *testing.T) {
	var res struct {
		Locations []efaLocation `json:"locations"`
	}
	load(t, "efa_stopfinder.json", &res)
	stops := NewEFA("naldo", NaldoBaseURL).parseStops(res.Locations, 3)
	if len(stops) != 3 || stops[0].ID != "naldo:de:08416:1006" || stops[0].Name != "Mössingen, Bahnhof" ||
		stops[0].Place != "Mössingen" || stops[0].Provider != "naldo" {
		t.Fatalf("stops = %+v", stops)
	}
	if !wants(stops[0].Products, Train) || !wants(stops[0].Products, Bus) {
		t.Errorf("products = %v", stops[0].Products)
	}
}

func TestEFADepartures(t *testing.T) {
	var res struct {
		StopEvents []efaStopEvent `json:"stopEvents"`
	}
	load(t, "efa_dm_hbf.json", &res)
	ds := parseEFADepartures(res.StopEvents)
	mex := ds[0]
	if mex.Line != "MEX18" || mex.Product != Train || mex.Platform != "3" || mex.Delay == nil || *mex.Delay != 70 ||
		mex.Planned.Format("15:04") != "06:33" || mex.Direction != "Stuttgart Hauptbahnhof (oben)" {
		t.Errorf("MEX18 = %+v", mex)
	}
	var x82 *Departure
	for i := range ds {
		if ds[i].Line == "X82" {
			x82 = &ds[i]
		}
	}
	if x82 == nil || x82.Product != Bus || x82.Platform != "M" {
		t.Errorf("X82 = %+v (platform without \"Bstg.\")", x82)
	}
	for _, d := range ds {
		for _, r := range d.Remarks {
			if strings.ContainsAny(r, "<>") {
				t.Errorf("HTML in a remark: %q", r)
			}
		}
	}

	load(t, "efa_dm_aula.json", &res)
	ds = parseEFADepartures(res.StopEvents)
	if ds[1].Line != "Bus 3" || ds[1].Platform != "3" || ds[1].Direction != "Tübingen WHO Ulmenweg" {
		t.Errorf("TüBus = %+v", ds[1])
	}
	cancelled := parseEFADepartures([]efaStopEvent{{DepartureTimePlanned: "2026-10-04T05:00:00Z",
		RealtimeStatus: []string{"TRIP_CANCELLED"}}, {DepartureTimePlanned: "2026-10-04T05:00:00Z", IsCancelled: true}})
	if !cancelled[0].Cancelled || !cancelled[1].Cancelled {
		t.Error("EFA cancellations")
	}
}

func TestEFAJourneys(t *testing.T) {
	var res struct {
		Journeys []efaJourney `json:"journeys"`
	}
	load(t, "efa_trip.json", &res)
	js := parseEFAJourneys(res.Journeys, true)
	if len(js) != 3 {
		t.Fatalf("%d journeys", len(js))
	}
	j := js[1]
	if len(j.Legs) != 1 || j.Legs[0].Line != "Bus 5" || j.Legs[0].Product != Bus || j.Transfers != 0 ||
		j.Legs[0].From != "Mössingen Bahnhof" || j.Legs[0].DepPlatform != "5" || j.Duration != 40 {
		t.Errorf("bus journey = %+v", j)
	}
	if len(j.Legs[0].Stops) != 25 {
		t.Errorf("stops = %d", len(j.Legs[0].Stops))
	}
	rb := js[2].Legs[0]
	if rb.Line != "RB66" || rb.Product != Train || len(rb.Remarks) == 0 ||
		!strings.Contains(rb.Remarks[0], "Schienenersatzverkehr") {
		t.Errorf("RB66 leg = %+v", rb)
	}
	text := (&JourneysResult{From: Stop{Name: "Mössingen"}, To: Stop{Name: "Reutlingen ZOB"}, Provider: "naldo",
		When: at("2026-10-05 07:00"), Journeys: withoutStops(js)}).Text()
	if !strings.Contains(text, "07:12 Mössingen Bahnhof (pl. 5) → 07:52 Reutlingen Stadtmitte (pl. A) · Bus 5") {
		t.Errorf("text:\n%s", text)
	}
}

func TestExclude(t *testing.T) {
	ex := efaExclude([]Product{Bus})
	for _, c := range ex {
		if efaProduct(c) == Bus {
			t.Errorf("bus class %d excluded", c)
		}
	}
	if len(ex) == 0 || efaExclude(nil) != nil {
		t.Error("exclusions")
	}
	if got := strings.Join(dbCodes([]Product{Train}), ","); got != "ICE,EC_IC,IR,REGIONAL" {
		t.Errorf("db codes = %s", got)
	}
}
