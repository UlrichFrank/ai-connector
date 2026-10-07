package transit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DBBaseURL is bahn.de's web API (the one www.bahn.de itself uses; also what db-vendo-client's "dbweb" profile
// talks to). The community REST API v6.db.transport.rest answered 503 when this was written (October 2026).
const DBBaseURL = "https://www.bahn.de/web/api"

// DB is Deutsche Bahn: every train in Germany (and most buses and trams, from the regional feeds) with real-time
// delays, platforms and cancellations.
type DB struct {
	Base string
	f    *fetcher
}

// NewDB makes the DB provider. base "" is DBBaseURL.
func NewDB(base string) *DB {
	if base == "" {
		base = DBBaseURL
	}
	// bahn.de's bot protection refuses requests that ask for compressed answers but not "identity" ones; we decompress
	// gzip ourselves when it is sent anyway. 300 ms between requests keeps us far below its limits.
	return &DB{Base: strings.TrimRight(base, "/"), f: newFetcher("db", 300*time.Millisecond,
		map[string]string{"Accept-Encoding": "identity", "Accept-Language": "de"})}
}

func (d *DB) Name() string { return "db" }

// dbProducts maps DB's product names (short and long forms) to ours.
var dbProducts = map[string]Product{
	"ICE": Train, "EC_IC": Train, "IR": Train, "REGIONAL": Train,
	"HOCHGESCHWINDIGKEITSZUEGE": Train, "INTERCITYUNDEUROCITYZUEGE": Train, "INTERREGIOUNDSCHNELLZUEGE": Train,
	"NAHVERKEHRSONSTIGEZUEGE": Train,
	"SBAHN":                   SBahn, "SBAHNEN": SBahn,
	"BUS": Bus, "BUSSE": Bus,
	"TRAM": Tram, "STRASSENBAHN": Tram,
	"UBAHN":  Subway,
	"SCHIFF": Ferry, "SCHIFFE": Ferry,
	"ANRUFPFLICHTIG": OnDemand, "ANRUFPFLICHTIGEVERKEHRE": OnDemand,
}

var dbFilter = map[Product][]string{
	Train: {"ICE", "EC_IC", "IR", "REGIONAL"}, SBahn: {"SBAHN"}, Bus: {"BUS"}, Tram: {"TRAM"}, Subway: {"UBAHN"},
	Ferry: {"SCHIFF"}, OnDemand: {"ANRUFPFLICHTIG"},
}

func dbProduct(s string) Product {
	if p, ok := dbProducts[s]; ok {
		return p
	}
	return Other
}

func dbCodes(products []Product) []string {
	if len(products) == 0 {
		products = FilterProducts
	}
	var out []string
	for _, p := range products {
		out = append(out, dbFilter[p]...)
	}
	return out
}

// dbTime parses DB's local times ("2026-10-04T06:33:00", sometimes with an offset).
func dbTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(Berlin), true
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, Berlin)
	return t, err == nil
}

func dbTimePtr(s string) *time.Time {
	if t, ok := dbTime(s); ok {
		return &t
	}
	return nil
}

func (d *DB) get(ctx context.Context, path string, q url.Values, out any) error {
	b, err := d.f.do(ctx, "GET", d.Base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return d.decode(b, out)
}

func (d *DB) decode(b []byte, out any) error {
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Provider: "db", Msg: "cannot read the answer (the API may have changed)"}
	}
	return nil
}

// --- stops

type dbLocation struct {
	ExtID    string   `json:"extId"`
	ID       string   `json:"id"`
	Lat      float64  `json:"lat"`
	Lon      float64  `json:"lon"`
	Name     string   `json:"name"`
	Products []string `json:"products"`
	Type     string   `json:"type"`
}

func (d *DB) FindStops(ctx context.Context, query string, limit int) ([]Stop, error) {
	var locs []dbLocation
	q := url.Values{"suchbegriff": {query}, "typ": {"ALL"}, "limit": {fmt.Sprint(max(limit*2, 10))}}
	if err := d.get(ctx, "/reiseloesung/orte", q, &locs); err != nil {
		return nil, err
	}
	return parseDBLocations(locs, limit), nil
}

func parseDBLocations(locs []dbLocation, limit int) []Stop {
	var out []Stop
	for _, l := range locs {
		id := cleanID(l.ExtID)
		if l.Type != "ST" || id == "" {
			continue
		}
		s := Stop{ID: "db:" + id, Name: Clean(l.Name, maxName), Lat: l.Lat, Lon: l.Lon, Provider: "db"}
		seen := map[Product]bool{}
		for _, p := range l.Products {
			if pr := dbProduct(p); !seen[pr] {
				seen[pr] = true
				s.Products = append(s.Products, pr)
			}
		}
		out = append(out, s)
		if len(out) == limit {
			break
		}
	}
	return out
}

// --- departures

type dbMeldung struct {
	Prioritaet string `json:"prioritaet"`
	Text       string `json:"text"`
	Type       string `json:"type"`
}

type dbVerkehrsmittel struct {
	Name           string `json:"name"`
	LinienNummer   string `json:"linienNummer"`
	KurzText       string `json:"kurzText"`
	MittelText     string `json:"mittelText"`
	LangText       string `json:"langText"`
	ProduktGattung string `json:"produktGattung"`
	Richtung       string `json:"richtung"`
	Typ            string `json:"typ"`
}

func (v dbVerkehrsmittel) line() string {
	for _, s := range []string{v.MittelText, v.LangText, v.Name, v.KurzText} {
		if s = Clean(s, 40); s != "" {
			return s
		}
	}
	return ""
}

type dbAbfahrt struct {
	Zeit          string           `json:"zeit"`
	EzZeit        string           `json:"ezZeit"`
	Gleis         string           `json:"gleis"`
	EzGleis       string           `json:"ezGleis"`
	Terminus      string           `json:"terminus"`
	JourneyID     string           `json:"journeyId"`
	Meldungen     []dbMeldung      `json:"meldungen"`
	Verkehrmittel dbVerkehrsmittel `json:"verkehrmittel"`
	Cancelled     bool             `json:"cancelled"`
	Canceled      bool             `json:"canceled"`
}

// cancelledBy says whether DB's notes say the stop or the trip is cancelled.
func cancelledBy(ms []dbMeldung) bool {
	for _, m := range ms {
		t := strings.ToLower(m.Text)
		if strings.Contains(m.Type, "AUSFALL") || strings.Contains(m.Type, "CANCEL") ||
			strings.Contains(t, "halt entfällt") || strings.Contains(t, "fällt aus") || strings.Contains(t, "fahrt entfällt") {
			return true
		}
	}
	return false
}

// importantNotes are the notes worth a line: real-time notes and those DB ranks above low priority.
func importantNotes(ms []dbMeldung) []string {
	var out []string
	for _, m := range ms {
		if m.Prioritaet == "NIEDRIG" {
			continue
		}
		out = append(out, m.Text)
	}
	return remarks(out)
}

func (d *DB) Departures(ctx context.Context, q DepartureQuery) ([]Departure, error) {
	if q.Duration <= 0 {
		q.Duration = time.Hour
	}
	end := q.When.Add(q.Duration)
	var all []Departure
	seen := map[string]bool{}
	at := q.When
	// one request covers about an hour or ~50 departures; ask again from the last one while the window is not full
	for range 3 {
		local := at.In(Berlin)
		v := url.Values{"ortExtId": {q.StopID}, "datum": {local.Format("2006-01-02")},
			"zeit": {local.Format("15:04:05")}, "mitVias": {"true"}, "maxVias": {"5"}}
		if len(q.Products) > 0 {
			v["verkehrsmittel[]"] = dbCodes(q.Products)
		}
		var res struct {
			Entries []dbAbfahrt `json:"entries"`
		}
		if err := d.get(ctx, "/reiseloesung/abfahrten", v, &res); err != nil {
			if len(all) > 0 {
				break
			}
			return nil, err
		}
		deps := parseDBDepartures(res.Entries)
		last := at
		added := 0
		for _, dep := range deps {
			key := dep.TripID + "|" + dep.Planned.String()
			if dep.Planned.After(last) {
				last = dep.Planned
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, dep)
			added++
		}
		if added == 0 || !last.Before(end) || (q.Limit > 0 && len(all) >= q.Limit*2) {
			break
		}
		at = last.Add(time.Minute)
	}
	return windowDepartures(all, q), nil
}

func parseDBDepartures(entries []dbAbfahrt) []Departure {
	var out []Departure
	for _, e := range entries {
		planned, ok := dbTime(e.Zeit)
		if !ok {
			continue
		}
		dep := Departure{
			Line:      e.Verkehrmittel.line(),
			Product:   dbProduct(e.Verkehrmittel.ProduktGattung),
			Direction: Clean(e.Terminus, maxName),
			Planned:   planned,
			Realtime:  dbTimePtr(e.EzZeit),
			Platform:  Clean(e.Gleis, 20),
			Cancelled: e.Cancelled || e.Canceled || cancelledBy(e.Meldungen),
			Remarks:   importantNotes(e.Meldungen),
			TripID:    dbTripID(e.JourneyID),
		}
		if p := Clean(e.EzGleis, 20); p != "" && p != dep.Platform {
			dep.PlannedPlatform, dep.Platform = dep.Platform, p
		}
		dep.Delay = delay(dep.Planned, dep.Realtime)
		out = append(out, dep)
	}
	return out
}

// windowDepartures keeps the departures of the window (by their real-time departure when known, so a delayed train
// planned before the window still shows), sorted, at most Limit, and of the asked products.
func windowDepartures(all []Departure, q DepartureQuery) []Departure {
	end := q.When.Add(q.Duration)
	var out []Departure
	for _, d := range all {
		t := d.Planned
		if d.Realtime != nil {
			t = *d.Realtime
		}
		if t.Before(q.When.Add(-time.Minute)) || !t.Before(end) || !wants(q.Products, d.Product) {
			continue
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return effective(out[i]).Before(effective(out[j])) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

func effective(d Departure) time.Time {
	if d.Realtime != nil {
		return *d.Realtime
	}
	return d.Planned
}

// --- journeys

type dbZeit struct {
	Sollzeit string `json:"sollzeit"`
	Echtzeit string `json:"echtzeit"`
}

type dbHalt struct {
	ExtID     string      `json:"extId"`
	Name      string      `json:"name"`
	Abfahrt   *dbZeit     `json:"abfahrt"`
	Ankunft   *dbZeit     `json:"ankunft"`
	Gleis     string      `json:"gleis"`
	EzGleis   string      `json:"ezGleis"`
	Cancelled bool        `json:"cancelled"`
	Meldungen []dbMeldung `json:"priorisierteMeldungen"`
	Kategorie string      `json:"kategorie"`
	Nummer    string      `json:"nummer"`
}

type dbRisNotiz struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type dbAbschnitt struct {
	AbfahrtsOrt          string           `json:"abfahrtsOrt"`
	AnkunftsOrt          string           `json:"ankunftsOrt"`
	Abfahrt              dbZeit           `json:"abfahrt"`
	Ankunft              dbZeit           `json:"ankunft"`
	Halte                []dbHalt         `json:"halte"`
	JourneyID            string           `json:"journeyId"`
	Verkehrsmittel       dbVerkehrsmittel `json:"verkehrsmittel"`
	OriginCancelled      bool             `json:"originCancelled"`
	DestinationCancelled bool             `json:"destinationCancelled"`
	Cancelled            bool             `json:"cancelled"`
	RisNotizen           []dbRisNotiz     `json:"risNotizen"`
	Meldungen            []dbMeldung      `json:"priorisierteMeldungen"`
}

type dbVerbindung struct {
	TripID     string        `json:"tripId"`
	Abschnitte []dbAbschnitt `json:"verbindungsAbschnitte"`
	Umstiege   int           `json:"umstiegsAnzahl"`
	Dauer      int           `json:"verbindungsDauerInSeconds"`
	EzDauer    int           `json:"ezVerbindungsDauerInSeconds"`
	RisNotizen []dbRisNotiz  `json:"risNotizen"`
	Meldungen  []dbMeldung   `json:"priorisierteMeldungen"`
	Cancelled  bool          `json:"cancelled"`
}

func dbStopID(id string) string { return "A=1@L=" + id + "@" }

// dbTripID is the id of a DB trip for the trip tool.
func dbTripID(journeyID string) string {
	if id := cleanID(journeyID); id != "" {
		return "db:" + id
	}
	return ""
}

func (d *DB) Journeys(ctx context.Context, q JourneyQuery) ([]Journey, error) {
	local := q.When.In(Berlin)
	body := map[string]any{
		"abfahrtsHalt":     dbStopID(q.From),
		"ankunftsHalt":     dbStopID(q.To),
		"anfrageZeitpunkt": local.Format("2006-01-02T15:04:05"),
		"ankunftSuche":     "ABFAHRT",
		"klasse":           "KLASSE_2",
		"produktgattungen": dbCodes(q.Products),
		"reisende": []any{map[string]any{"typ": "ERWACHSENER", "alter": []any{}, "anzahl": 1,
			"ermaessigungen": []any{map[string]any{"art": "KEINE_ERMAESSIGUNG", "klasse": "KLASSENLOS"}}}},
		"schnelleVerbindungen":              true,
		"sitzplatzOnly":                     false,
		"bikeCarriage":                      false,
		"reservierungsKontingenteVorhanden": false,
		"nurDeutschlandTicketVerbindungen":  false,
		"deutschlandTicketVorhanden":        false,
	}
	if q.ArriveBy {
		body["ankunftSuche"] = "ANKUNFT"
	}
	if q.Via != "" {
		body["zwischenhalte"] = []any{map[string]any{"id": dbStopID(q.Via)}}
	}
	b, _ := json.Marshal(body)
	raw, err := d.f.do(ctx, "POST", d.Base+"/angebote/fahrplan", b)
	if err != nil {
		return nil, err
	}
	var res struct {
		Verbindungen []dbVerbindung `json:"verbindungen"`
	}
	if err := d.decode(raw, &res); err != nil {
		return nil, err
	}
	// all of them (DB sends about five): the Service keeps Results of those in the window
	return parseDBJourneys(res.Verbindungen, true), nil
}

func parseDBJourneys(vs []dbVerbindung, withStops bool) []Journey {
	var out []Journey
	for _, v := range vs {
		var j Journey
		for _, a := range v.Abschnitte {
			if leg, ok := parseDBLeg(a, withStops); ok {
				j.Legs = append(j.Legs, leg)
			}
		}
		if len(j.Legs) == 0 {
			continue
		}
		j.Transfers = v.Umstiege
		j.Duration = journeyMinutes(j.Legs)
		var notes []string
		for _, n := range v.RisNotizen {
			notes = append(notes, n.Value)
		}
		notes = append(notes, importantNotes(v.Meldungen)...)
		j.Remarks = remarks(notes)
		out = append(out, j)
	}
	return out
}

// journeyMinutes is first departure to last arrival (real-time when known).
func journeyMinutes(legs []Leg) int {
	dep, arr := legs[0].Dep, legs[len(legs)-1].Arr
	if legs[0].DepRealtime != nil {
		dep = *legs[0].DepRealtime
	}
	if l := legs[len(legs)-1]; l.ArrRealtime != nil {
		arr = *l.ArrRealtime
	}
	return int(arr.Sub(dep).Round(time.Minute) / time.Minute)
}

func parseDBLeg(a dbAbschnitt, withStops bool) (Leg, bool) {
	dep, ok1 := dbTime(a.Abfahrt.Sollzeit)
	arr, ok2 := dbTime(a.Ankunft.Sollzeit)
	if !ok1 || !ok2 {
		return Leg{}, false
	}
	vm := a.Verkehrsmittel
	leg := Leg{
		From: Clean(a.AbfahrtsOrt, maxName), To: Clean(a.AnkunftsOrt, maxName),
		Dep: dep, DepRealtime: dbTimePtr(a.Abfahrt.Echtzeit),
		Arr: arr, ArrRealtime: dbTimePtr(a.Ankunft.Echtzeit),
		Cancelled: a.Cancelled || a.OriginCancelled || a.DestinationCancelled,
	}
	switch vm.Typ {
	case "WALK", "TRANSFER", "FUSSWEG":
		leg.Walking, leg.Product = true, Walk
	default:
		leg.Line = vm.line()
		leg.Product = dbProduct(vm.ProduktGattung)
		leg.Direction = Clean(vm.Richtung, maxName)
		leg.TripID = dbTripID(a.JourneyID)
	}
	if n := len(a.Halte); n > 0 && !leg.Walking {
		leg.DepPlatform = platform(a.Halte[0].Gleis, a.Halte[0].EzGleis)
		leg.ArrPlatform = platform(a.Halte[n-1].Gleis, a.Halte[n-1].EzGleis)
		if withStops {
			for _, h := range a.Halte {
				leg.Stops = append(leg.Stops, dbStopTime(h))
			}
		}
	}
	var notes []string
	for _, n := range a.RisNotizen {
		notes = append(notes, n.Value)
	}
	notes = append(notes, importantNotes(a.Meldungen)...)
	leg.Remarks = remarks(notes)
	if cancelledBy(a.Meldungen) {
		leg.Cancelled = true
	}
	leg.DepDelay = delay(leg.Dep, leg.DepRealtime)
	leg.ArrDelay = delay(leg.Arr, leg.ArrRealtime)
	return leg, true
}

// platform is the real-time platform when it differs ("5 (instead of 4)").
func platform(planned, rt string) string {
	planned, rt = Clean(planned, 20), Clean(rt, 20)
	if rt != "" && rt != planned {
		if planned == "" {
			return rt
		}
		return rt + " (instead of " + planned + ")"
	}
	return planned
}

func dbStopTime(h dbHalt) StopTime {
	st := StopTime{Name: Clean(h.Name, maxName), Platform: platform(h.Gleis, h.EzGleis),
		Cancelled: h.Cancelled || cancelledBy(h.Meldungen)}
	if h.Ankunft != nil {
		st.Arr, st.ArrRealtime = dbTimePtr(h.Ankunft.Sollzeit), dbTimePtr(h.Ankunft.Echtzeit)
	}
	if h.Abfahrt != nil {
		st.Dep, st.DepRealtime = dbTimePtr(h.Abfahrt.Sollzeit), dbTimePtr(h.Abfahrt.Echtzeit)
	}
	return st
}

// --- trip

type dbFahrt struct {
	ZugName    string       `json:"zugName"`
	Halte      []dbHalt     `json:"halte"`
	Cancelled  bool         `json:"cancelled"`
	RisNotizen []dbRisNotiz `json:"risNotizen"`
	Meldungen  []dbMeldung  `json:"priorisierteMeldungen"`
}

func (d *DB) Trip(ctx context.Context, id string) (*Trip, error) {
	var f dbFahrt
	if err := d.get(ctx, "/reiseloesung/fahrt", url.Values{"journeyId": {id}, "poly": {"false"}}, &f); err != nil {
		return nil, err
	}
	return parseDBTrip(id, f), nil
}

func parseDBTrip(id string, f dbFahrt) *Trip {
	t := &Trip{ID: "db:" + cleanID(id), Cancelled: f.Cancelled}
	for _, h := range f.Halte {
		t.Stops = append(t.Stops, dbStopTime(h))
	}
	if len(f.Halte) > 0 {
		h := f.Halte[0]
		t.Line = Clean(strings.TrimSpace(h.Kategorie+" "+h.Nummer), 40)
		t.Direction = Clean(f.Halte[len(f.Halte)-1].Name, maxName)
	}
	if t.Line == "" {
		t.Line = Clean(f.ZugName, 40)
	}
	var notes []string
	for _, n := range f.RisNotizen {
		notes = append(notes, n.Value)
	}
	for _, m := range f.Meldungen {
		notes = append(notes, m.Text) // all of them: the user asked for this trip's details
	}
	t.Remarks = remarks(notes)
	return t
}
