package transit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// EFA-BW instances (Mentz EFA, rapidJSON). Both are the same backend of NVBW (Baden-Württemberg); "naldo" is the
// instance naldo.de's journey planner uses (its page proxies to it), "nvbw" the statewide one behind efa-bw.de and
// bwegt. They answer with the same stops, lines and real-time data (checked October 2026).
const (
	EFABWBaseURL = "https://www.efa-bw.de/nvbw/"
	NaldoBaseURL = "https://www.efa-bw.de/naldo/"
)

// EFA is a Mentz EFA server (rapidJSON output).
type EFA struct {
	name string
	Base string
	f    *fetcher
}

// NewEFA makes an EFA provider called name (the prefix of its ids) for the server at base.
func NewEFA(name, base string) *EFA {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return &EFA{name: name, Base: base, f: newFetcher(name, 200*time.Millisecond, nil)}
}

func (e *EFA) Name() string { return e.name }

// efaProduct maps EFA's product classes (motType).
func efaProduct(class int) Product {
	switch class {
	case 0, 13, 14, 15, 16, 18:
		return Train
	case 1:
		return SBahn
	case 2:
		return Subway
	case 3, 4:
		return Tram
	case 5, 6, 7, 17, 19:
		return Bus
	case 9:
		return Ferry
	case 10:
		return OnDemand
	case 97, 98, 99, 100:
		return Walk
	}
	return Other
}

func (e *EFA) get(ctx context.Context, endpoint string, q url.Values, out any) error {
	q.Set("outputFormat", "rapidJSON")
	q.Set("coordOutputFormat", "WGS84[dd.ddddd]")
	b, err := e.f.do(ctx, "GET", e.Base+endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Provider: e.name, Msg: "cannot read the answer (the API may have changed)"}
	}
	return nil
}

func efaTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.In(Berlin)
	return &t
}

// --- stops

type efaParent struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Type   string     `json:"type"`
	Parent *efaParent `json:"parent"`
}

type efaLocation struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	DisassembledName string     `json:"disassembledName"`
	Type             string     `json:"type"`
	Coord            []float64  `json:"coord"`
	MatchQuality     int        `json:"matchQuality"`
	IsBest           bool       `json:"isBest"`
	ProductClasses   []int      `json:"productClasses"`
	Parent           *efaParent `json:"parent"`
	Properties       struct {
		Platform            string `json:"platform"`
		PlatformName        string `json:"platformName"`
		PlannedPlatformName string `json:"plannedPlatformName"`
	} `json:"properties"`
	DepartureTimePlanned   string   `json:"departureTimePlanned"`
	DepartureTimeEstimated string   `json:"departureTimeEstimated"`
	ArrivalTimePlanned     string   `json:"arrivalTimePlanned"`
	ArrivalTimeEstimated   string   `json:"arrivalTimeEstimated"`
	IsCancelled            bool     `json:"isCancelled"`
	RealtimeStatus         []string `json:"realtimeStatus"`
}

// stopName is the name of the stop (not of the platform).
func (l efaLocation) stopName() string {
	if l.Type == "platform" && l.Parent != nil && l.Parent.Type == "stop" && l.Parent.Name != "" {
		return Clean(l.Parent.Name, maxName)
	}
	return Clean(l.Name, maxName)
}

func (l efaLocation) platform() string {
	p := l.Properties
	cur := p.PlatformName
	if cur == "" {
		cur = p.Platform
	}
	if cur == "" && l.Type == "platform" {
		cur = l.DisassembledName
	}
	return platform(bare(firstNonEmpty(p.PlannedPlatformName, cur)), bare(cur))
}

// bare drops EFA's "Bstg. " / "Gleis " in front of a platform.
func bare(p string) string {
	for _, pre := range []string{"Bstg. ", "Bstg ", "Gleis ", "Gl. "} {
		p = strings.TrimPrefix(p, pre)
	}
	return p
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func (e *EFA) FindStops(ctx context.Context, query string, limit int) ([]Stop, error) {
	var res struct {
		Locations []efaLocation `json:"locations"`
	}
	q := url.Values{"type_sf": {"any"}, "name_sf": {query}, "anyObjFilter_sf": {"2"}, "anyMaxSizeHitList": {"30"}}
	if err := e.get(ctx, "XML_STOPFINDER_REQUEST", q, &res); err != nil {
		return nil, err
	}
	return e.parseStops(res.Locations, limit), nil
}

func (e *EFA) parseStops(locs []efaLocation, limit int) []Stop {
	sort.SliceStable(locs, func(i, j int) bool {
		if locs[i].IsBest != locs[j].IsBest {
			return locs[i].IsBest
		}
		return locs[i].MatchQuality > locs[j].MatchQuality
	})
	var out []Stop
	for _, l := range locs {
		id := cleanID(l.ID)
		if l.Type != "stop" || id == "" {
			continue
		}
		s := Stop{ID: e.name + ":" + id, Name: Clean(l.Name, maxName), Provider: e.name}
		if l.Parent != nil && l.Parent.Type == "locality" {
			s.Place = Clean(l.Parent.Name, maxName)
		}
		if len(l.Coord) == 2 {
			s.Lat, s.Lon = l.Coord[0], l.Coord[1]
		}
		seen := map[Product]bool{}
		for _, c := range l.ProductClasses {
			if p := efaProduct(c); !seen[p] && p != Other && p != Walk {
				seen[p] = true
				s.Products = append(s.Products, p)
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

type efaTransportation struct {
	Name             string `json:"name"`
	DisassembledName string `json:"disassembledName"`
	Number           string `json:"number"`
	Product          struct {
		Class int    `json:"class"`
		Name  string `json:"name"`
	} `json:"product"`
	Destination struct {
		Name string `json:"name"`
	} `json:"destination"`
}

func (t efaTransportation) line() string {
	for _, s := range []string{t.DisassembledName, t.Number, t.Name} {
		if s = Clean(s, 40); s != "" {
			p := efaProduct(t.Product.Class)
			// a bare number says little: "Bus 7615", "Tram 4"
			if strings.Trim(s, "0123456789") == "" {
				switch p {
				case Bus, Tram, OnDemand, Ferry:
					return map[Product]string{Bus: "Bus", Tram: "Tram", OnDemand: "Rufbus", Ferry: "Ferry"}[p] + " " + s
				}
			}
			return s
		}
	}
	return Clean(t.Product.Name, 40)
}

type efaInfo struct {
	Type      string `json:"type"`
	Priority  string `json:"priority"`
	InfoLinks []struct {
		Subtitle string `json:"subtitle"`
		URLText  string `json:"urlText"`
	} `json:"infoLinks"`
}

type efaHint struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// efaRemarks are real-time hints and important disruption notices (never their HTML bodies: only the titles).
func efaRemarks(infos []efaInfo, hints []efaHint) []string {
	var out []string
	for _, h := range hints {
		if strings.HasPrefix(h.Type, "RT") {
			out = append(out, h.Content)
		}
	}
	for _, i := range infos {
		if i.Priority != "high" && i.Priority != "veryHigh" {
			continue
		}
		for _, l := range i.InfoLinks {
			out = append(out, firstNonEmpty(l.Subtitle, l.URLText))
		}
	}
	return remarks(out)
}

// monitored: EFA has real-time data for this trip. Without it, EFA still fills the "estimated" times (with the
// planned ones), which would read as "on time".
func monitored(controlled bool, status []string) bool {
	return controlled || slices.Contains(status, "MONITORED")
}

// estimated is the real-time time when the trip is monitored.
func estimated(ok bool, s string) *time.Time {
	if !ok {
		return nil
	}
	return efaTime(s)
}

func cancelledStatus(status []string) bool {
	for _, s := range status {
		if strings.Contains(s, "CANCELLED") {
			return true
		}
	}
	return false
}

type efaStopEvent struct {
	Location               efaLocation       `json:"location"`
	DepartureTimePlanned   string            `json:"departureTimePlanned"`
	DepartureTimeEstimated string            `json:"departureTimeEstimated"`
	Transportation         efaTransportation `json:"transportation"`
	RealtimeStatus         []string          `json:"realtimeStatus"`
	IsRealtimeControlled   bool              `json:"isRealtimeControlled"`
	IsCancelled            bool              `json:"isCancelled"`
	Infos                  []efaInfo         `json:"infos"`
	Hints                  []efaHint         `json:"hints"`
}

func (e *EFA) Departures(ctx context.Context, q DepartureQuery) ([]Departure, error) {
	if q.Duration <= 0 {
		q.Duration = time.Hour
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	n := limit * 2
	if len(q.Products) > 0 {
		n = limit * 4
	}
	n = min(max(n, 20), 120)
	local := q.When.In(Berlin)
	v := url.Values{"type_dm": {"any"}, "name_dm": {q.StopID}, "mode": {"direct"}, "useRealtime": {"1"},
		"depType": {"stopEvents"}, "useAllStops": {"1"}, "limit": {fmt.Sprint(n)},
		"itdDate": {local.Format("20060102")}, "itdTime": {local.Format("1504")}}
	var res struct {
		StopEvents []efaStopEvent `json:"stopEvents"`
	}
	if err := e.get(ctx, "XML_DM_REQUEST", v, &res); err != nil {
		return nil, err
	}
	return windowDepartures(parseEFADepartures(res.StopEvents), q), nil
}

func parseEFADepartures(evs []efaStopEvent) []Departure {
	var out []Departure
	for _, ev := range evs {
		planned := efaTime(ev.DepartureTimePlanned)
		if planned == nil {
			continue
		}
		t := ev.Transportation
		d := Departure{
			Line:      t.line(),
			Product:   efaProduct(t.Product.Class),
			Direction: Clean(t.Destination.Name, maxName),
			Planned:   *planned,
			Realtime:  estimated(monitored(ev.IsRealtimeControlled, ev.RealtimeStatus), ev.DepartureTimeEstimated),
			Platform:  ev.Location.platform(),
			Cancelled: ev.IsCancelled || cancelledStatus(ev.RealtimeStatus),
			Remarks:   efaRemarks(ev.Infos, ev.Hints),
		}
		d.Delay = delay(d.Planned, d.Realtime)
		out = append(out, d)
	}
	return out
}

// --- journeys

type efaLeg struct {
	Duration       int               `json:"duration"`
	Origin         efaLocation       `json:"origin"`
	Destination    efaLocation       `json:"destination"`
	Transportation efaTransportation `json:"transportation"`
	StopSequence   []efaLocation     `json:"stopSequence"`
	Infos          []efaInfo         `json:"infos"`
	Hints          []efaHint         `json:"hints"`
	RealtimeStatus []string          `json:"realtimeStatus"`
	IsRealtime     bool              `json:"isRealtimeControlled"`
	IsCancelled    bool              `json:"isCancelled"`
}

type efaJourney struct {
	Interchanges int      `json:"interchanges"`
	Legs         []efaLeg `json:"legs"`
}

// efaExclude are EFA's means of transport (exclMOT_n) to leave out for the products wanted.
func efaExclude(products []Product) []int {
	if len(products) == 0 {
		return nil
	}
	var out []int
	for c := 0; c <= 19; c++ {
		p := efaProduct(c)
		if p != Other && !wants(products, p) {
			out = append(out, c)
		}
	}
	return out
}

func (e *EFA) Journeys(ctx context.Context, q JourneyQuery) ([]Journey, error) {
	local := q.When.In(Berlin)
	n := q.Results
	if n <= 0 {
		n = 4
	}
	v := url.Values{"type_origin": {"any"}, "name_origin": {q.From}, "type_destination": {"any"},
		"name_destination": {q.To}, "itdDate": {local.Format("20060102")}, "itdTime": {local.Format("1504")},
		"itdTripDateTimeDepArr": {"dep"}, "calcNumberOfTrips": {fmt.Sprint(n + 1)}, "useRealtime": {"1"},
		"locationServerActive": {"1"}, "routeType": {"LEASTTIME"}}
	if q.ArriveBy {
		v.Set("itdTripDateTimeDepArr", "arr")
	}
	if q.Via != "" {
		v.Set("type_via", "any")
		v.Set("name_via", q.Via)
	}
	if ex := efaExclude(q.Products); len(ex) > 0 {
		v.Set("excludedMeans", "checkbox")
		for _, c := range ex {
			v.Set(fmt.Sprintf("exclMOT_%d", c), "1")
		}
	}
	var res struct {
		Journeys []efaJourney `json:"journeys"`
	}
	if err := e.get(ctx, "XML_TRIP_REQUEST2", v, &res); err != nil {
		return nil, err
	}
	// one more than asked: the Service drops one that leaves before the asked time and keeps Results
	return parseEFAJourneys(res.Journeys, true), nil
}

func parseEFAJourneys(in []efaJourney, withStops bool) []Journey {
	var out []Journey
	for _, j := range in {
		var jj Journey
		for _, l := range j.Legs {
			if leg, ok := parseEFALeg(l, withStops); ok {
				jj.Legs = append(jj.Legs, leg)
			}
		}
		if len(jj.Legs) == 0 {
			continue
		}
		jj.Transfers = j.Interchanges
		jj.Duration = journeyMinutes(jj.Legs)
		out = append(out, jj)
	}
	return out
}

func parseEFALeg(l efaLeg, withStops bool) (Leg, bool) {
	dep := efaTime(l.Origin.DepartureTimePlanned)
	arr := efaTime(l.Destination.ArrivalTimePlanned)
	if dep == nil || arr == nil {
		return Leg{}, false
	}
	p := efaProduct(l.Transportation.Product.Class)
	rt := monitored(l.IsRealtime, l.RealtimeStatus)
	leg := Leg{
		Product: p, From: l.Origin.stopName(), To: l.Destination.stopName(),
		Dep: *dep, DepRealtime: estimated(rt, l.Origin.DepartureTimeEstimated),
		Arr: *arr, ArrRealtime: estimated(rt, l.Destination.ArrivalTimeEstimated),
		Cancelled: l.IsCancelled || cancelledStatus(l.RealtimeStatus) || l.Origin.IsCancelled ||
			l.Destination.IsCancelled,
		Remarks: efaRemarks(l.Infos, l.Hints),
	}
	if p == Walk {
		leg.Walking = true
	} else {
		leg.Line = l.Transportation.line()
		leg.Direction = Clean(l.Transportation.Destination.Name, maxName)
		leg.DepPlatform = l.Origin.platform()
		leg.ArrPlatform = l.Destination.platform()
		if withStops {
			for _, s := range l.StopSequence {
				leg.Stops = append(leg.Stops, StopTime{
					Name: s.stopName(), Platform: s.platform(),
					Arr: efaTime(s.ArrivalTimePlanned), ArrRealtime: estimated(rt, s.ArrivalTimeEstimated),
					Dep: efaTime(s.DepartureTimePlanned), DepRealtime: estimated(rt, s.DepartureTimeEstimated),
					Cancelled: s.IsCancelled || cancelledStatus(s.RealtimeStatus),
				})
			}
		}
	}
	leg.DepDelay = delay(leg.Dep, leg.DepRealtime)
	leg.ArrDelay = delay(leg.Arr, leg.ArrRealtime)
	return leg, true
}

// Trip: EFA has no stable trip id in its departures; the details of a journey come from the journeys answer itself
// (the Service keeps them).
func (e *EFA) Trip(context.Context, string) (*Trip, error) { return nil, ErrUnsupported }
