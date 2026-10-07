package transit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Cache lifetimes.
const (
	StopTTL      = 24 * time.Hour
	DepartureTTL = 30 * time.Second
	JourneyTTL   = 60 * time.Second
	// SavedTTL is how long a journey id stays valid for the trip tool.
	SavedTTL = 6 * time.Hour
)

// Service answers the tools: it picks the provider, resolves stop names, caches, and falls back from DB to EFA.
type Service struct {
	providers map[string]Provider
	now       func() time.Time

	stops    *cache[[]Stop]
	known    *cache[Stop] // stops by id, to name an id the model passes back
	deps     *cache[[]Departure]
	journeys *cache[[]Journey]
	saved    *cache[Journey]
	trips    *cache[string] // short trip ids ("t-…") → the provider's long ones
}

// NewService makes a service with the providers (by Name). now nil is time.Now.
func NewService(now func() time.Time, providers ...Provider) *Service {
	if now == nil {
		now = time.Now
	}
	s := &Service{providers: map[string]Provider{}, now: now,
		stops: newCache[[]Stop](StopTTL, 2000, now), known: newCache[Stop](StopTTL, 5000, now),
		deps: newCache[[]Departure](DepartureTTL, 500, now), journeys: newCache[[]Journey](JourneyTTL, 200, now),
		saved: newCache[Journey](SavedTTL, 2000, now), trips: newCache[string](SavedTTL, 20000, now)}
	for _, p := range providers {
		s.providers[p.Name()] = p
	}
	return s
}

// Default is the service with the public providers: db (bahn.de), efa (EFA-BW, statewide) and naldo (EFA-BW as
// naldo's journey planner uses it).
func Default() *Service {
	return NewService(nil, NewDB(""), NewEFA("efa", EFABWBaseURL), NewEFA("naldo", NaldoBaseURL))
}

// Now is the service's clock in Europe/Berlin.
func (s *Service) Now() time.Time { return s.now().In(Berlin) }

// ProviderNames are the configured providers, sorted.
func (s *Service) ProviderNames() []string {
	var out []string
	for n := range s.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// candidates are the providers to try, in order: the one asked for, or DB first (trains, all of Germany) and EFA
// after it, but EFA first when only buses/trams/subways are wanted.
func (s *Service) candidates(provider string, products []Product) ([]string, error) {
	if provider != "" {
		if _, ok := s.providers[provider]; !ok {
			return nil, fmt.Errorf("unknown provider %q (use %s)", provider, strings.Join(s.ProviderNames(), ", "))
		}
		return []string{provider}, nil
	}
	local := len(products) > 0
	for _, p := range products {
		if p == Train || p == SBahn {
			local = false
		}
	}
	var order []string
	if local {
		order = []string{"efa", "naldo", "db"}
	} else {
		order = []string{"db", "efa", "naldo"}
	}
	var out []string
	for _, n := range order {
		if _, ok := s.providers[n]; ok {
			out = append(out, n)
		}
	}
	// EFA and naldo are one backend: trying both helps nothing
	if slices.Contains(out, "efa") && slices.Contains(out, "naldo") {
		out = slices.DeleteFunc(out, func(n string) bool { return n == "naldo" })
	}
	for n := range s.providers {
		if !slices.Contains(out, n) && !slices.Contains([]string{"efa", "naldo", "db"}, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// fallbackOK says whether to try the next provider after err.
func fallbackOK(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Unavailable || errors.Is(err, ErrUnsupported)
}

// FindStopsResult is the answer of find_stop.
type FindStopsResult struct {
	Query    string   `json:"query"`
	Provider string   `json:"provider"`
	Stops    []Stop   `json:"stops"`
	Notes    []string `json:"notes,omitempty"`
}

// FindStops finds stops by name.
func (s *Service) FindStops(ctx context.Context, query, provider string, limit int) (*FindStopsResult, error) {
	query = Clean(query, 100)
	if query == "" {
		return nil, errors.New("query is empty")
	}
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	cands, err := s.candidates(provider, nil)
	if err != nil {
		return nil, err
	}
	res := &FindStopsResult{Query: query}
	var lastErr error
	for _, p := range cands {
		stops, err := s.findStops(ctx, p, query, limit)
		if err != nil {
			lastErr = err
			res.Notes = append(res.Notes, err.Error())
			if fallbackOK(err) {
				continue
			}
			return nil, err
		}
		res.Provider = p
		res.Stops = stops
		if len(stops) > 0 {
			return res, nil
		}
		res.Notes = append(res.Notes, fmt.Sprintf("%s found no stop named %q", p, query))
	}
	if len(res.Stops) == 0 && lastErr != nil && res.Provider == "" {
		return nil, lastErr
	}
	return res, nil
}

func (s *Service) findStops(ctx context.Context, provider, query string, limit int) ([]Stop, error) {
	key := provider + "|" + strings.ToLower(query) + "|" + fmt.Sprint(limit)
	if v, ok := s.stops.get(key); ok {
		return v, nil
	}
	stops, err := s.providers[provider].FindStops(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	s.stops.put(key, stops)
	for _, st := range stops {
		s.known.put(st.ID, st)
	}
	return stops, nil
}

var (
	dbIDRE  = regexp.MustCompile(`^[0-9]{6,9}$`)
	efaIDRE = regexp.MustCompile(`^de:[0-9]{5}:[0-9A-Za-z_:]+$`)
)

// splitID splits "db:8000141" into the provider and its id ("", "" when it is not an id of a known provider).
func (s *Service) splitID(in string) (string, string) {
	p, raw, ok := strings.Cut(in, ":")
	if !ok {
		return "", ""
	}
	if _, known := s.providers[p]; !known || raw == "" {
		return "", ""
	}
	return p, raw
}

// resolve finds the stop in provider for a name or an id; note says which stop a name was taken as.
func (s *Service) resolve(ctx context.Context, provider, in string) (Stop, string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return Stop{}, "", errors.New("a stop is missing")
	}
	if p, raw := s.splitID(in); p != "" {
		if p == provider {
			st, ok := s.known.get(in)
			if !ok {
				st = Stop{ID: in, Name: in, Provider: p}
			}
			_ = raw
			return st, "", nil
		}
		// an id of another provider: look the stop up by its name here
		st, ok := s.known.get(in)
		if !ok {
			return Stop{}, "", fmt.Errorf("%s is a %s id; give the stop's name to search it in %s", in, p, provider)
		}
		in = st.Name
	}
	switch {
	case provider == "db" && dbIDRE.MatchString(in), (provider == "efa" || provider == "naldo") && efaIDRE.MatchString(in):
		id := provider + ":" + in
		st, ok := s.known.get(id)
		if !ok {
			st = Stop{ID: id, Name: in, Provider: provider}
		}
		return st, "", nil
	}
	query := Clean(in, 100)
	stops, err := s.findStops(ctx, provider, query, 5)
	if err != nil {
		return Stop{}, "", err
	}
	if len(stops) == 0 {
		return Stop{}, "", &notFound{provider: provider, query: query}
	}
	st := stops[0]
	note := fmt.Sprintf("%q → %s (%s)", query, st.Name, st.ID)
	if len(stops) > 1 {
		var alt []string
		for _, o := range stops[1:min(len(stops), 3)] {
			alt = append(alt, o.Name)
		}
		note += "; other matches: " + strings.Join(alt, ", ")
	}
	return st, note, nil
}

type notFound struct{ provider, query string }

func (e *notFound) Error() string {
	return fmt.Sprintf("%s found no stop named %q", e.provider, e.query)
}

func rawID(id string) string {
	_, raw, _ := strings.Cut(id, ":")
	return raw
}

// DepartureArgs are the arguments of departures.
type DepartureArgs struct {
	Stop     string
	When     time.Time
	Duration time.Duration
	Products []Product
	Limit    int
	Provider string
}

// DeparturesResult is the answer of departures.
type DeparturesResult struct {
	Stop       Stop        `json:"stop"`
	Provider   string      `json:"provider"`
	When       time.Time   `json:"when"`
	Minutes    int         `json:"durationMinutes"`
	Departures []Departure `json:"departures"`
	Notes      []string    `json:"notes,omitempty"`
}

// Departures lists departures at a stop.
func (s *Service) Departures(ctx context.Context, a DepartureArgs) (*DeparturesResult, error) {
	if a.Duration <= 0 {
		a.Duration = time.Hour
	}
	if a.Limit <= 0 {
		a.Limit = 15
	}
	provider := a.Provider
	if p, _ := s.splitID(strings.TrimSpace(a.Stop)); p != "" && provider == "" {
		// an id: its provider first, the others only as a fallback
		provider = p
	}
	cands, err := s.candidates(a.Provider, a.Products)
	if err != nil {
		return nil, err
	}
	if provider != "" && a.Provider == "" {
		cands = append([]string{provider}, slices.DeleteFunc(cands, func(n string) bool { return n == provider })...)
	}
	var notes []string
	var lastErr error
	for i, p := range cands {
		st, note, err := s.resolve(ctx, p, a.Stop)
		if note != "" {
			notes = append(notes, "stop "+note)
		}
		if err == nil {
			var deps []Departure
			deps, err = s.departures(ctx, p, rawID(st.ID), a)
			if err == nil && (len(deps) > 0 || i == len(cands)-1) {
				return &DeparturesResult{Stop: st, Provider: p, When: a.When, Minutes: int(a.Duration / time.Minute),
					Departures: deps, Notes: notes}, nil
			}
			if err == nil {
				err = fmt.Errorf("%s has no departures at %s in this window", p, st.Name)
			}
		}
		lastErr = err
		var nf *notFound
		if i < len(cands)-1 && (fallbackOK(err) || errors.As(err, &nf) || strings.Contains(err.Error(), "no departures")) {
			notes = append(notes, err.Error()+"; trying "+cands[i+1])
			continue
		}
		break
	}
	return nil, lastErr
}

func (s *Service) departures(ctx context.Context, provider, stopID string, a DepartureArgs) ([]Departure, error) {
	key := fmt.Sprintf("%s|%s|%d|%v|%v|%d", provider, stopID, a.When.Unix()/30, a.Duration, a.Products, a.Limit)
	if v, ok := s.deps.get(key); ok {
		return v, nil
	}
	deps, err := s.providers[provider].Departures(ctx, DepartureQuery{StopID: stopID, When: a.When,
		Duration: a.Duration, Products: a.Products, Limit: a.Limit})
	if err != nil {
		return nil, err
	}
	for i := range deps {
		deps[i].TripID = s.alias(deps[i].TripID)
	}
	s.deps.put(key, deps)
	return deps, nil
}

// JourneyArgs are the arguments of journeys.
type JourneyArgs struct {
	From, To, Via string
	When          time.Time
	ArriveBy      bool
	Results       int
	Products      []Product
	Provider      string
}

// JourneysResult is the answer of journeys.
type JourneysResult struct {
	From     Stop      `json:"from"`
	To       Stop      `json:"to"`
	Via      *Stop     `json:"via,omitempty"`
	Provider string    `json:"provider"`
	When     time.Time `json:"when"`
	ArriveBy bool      `json:"arriveBy,omitempty"`
	Journeys []Journey `json:"journeys"`
	Notes    []string  `json:"notes,omitempty"`
}

// Journeys plans journeys.
func (s *Service) Journeys(ctx context.Context, a JourneyArgs) (*JourneysResult, error) {
	if a.Results <= 0 || a.Results > 6 {
		a.Results = 4
	}
	cands, err := s.candidates(a.Provider, a.Products)
	if err != nil {
		return nil, err
	}
	if p, _ := s.splitID(strings.TrimSpace(a.From)); p != "" && a.Provider == "" {
		cands = append([]string{p}, slices.DeleteFunc(cands, func(n string) bool { return n == p })...)
	}
	var notes []string
	var lastErr error
	for i, p := range cands {
		res, err := s.journeysWith(ctx, p, a, &notes)
		if err == nil && (len(res.Journeys) > 0 || i == len(cands)-1) {
			res.Notes = notes
			return res, nil
		}
		if err == nil {
			err = fmt.Errorf("%s found no journey", p)
		}
		lastErr = err
		var nf *notFound
		if i < len(cands)-1 && (fallbackOK(err) || errors.As(err, &nf) || strings.HasSuffix(err.Error(), "found no journey")) {
			notes = append(notes, err.Error()+"; trying "+cands[i+1])
			continue
		}
		break
	}
	return nil, lastErr
}

func (s *Service) journeysWith(ctx context.Context, p string, a JourneyArgs, notes *[]string) (*JourneysResult, error) {
	res := &JourneysResult{Provider: p, When: a.When, ArriveBy: a.ArriveBy}
	var err error
	var note string
	if res.From, note, err = s.resolve(ctx, p, a.From); err != nil {
		return nil, err
	} else if note != "" {
		*notes = append(*notes, "from "+note)
	}
	if res.To, note, err = s.resolve(ctx, p, a.To); err != nil {
		return nil, err
	} else if note != "" {
		*notes = append(*notes, "to "+note)
	}
	q := JourneyQuery{From: rawID(res.From.ID), To: rawID(res.To.ID), When: a.When, ArriveBy: a.ArriveBy,
		Results: a.Results, Products: a.Products}
	if strings.TrimSpace(a.Via) != "" {
		via, note, err := s.resolve(ctx, p, a.Via)
		if err != nil {
			return nil, err
		}
		if note != "" {
			*notes = append(*notes, "via "+note)
		}
		res.Via, q.Via = &via, rawID(via.ID)
	}
	key := fmt.Sprintf("%s|%s|%s|%s|%d|%v|%d|%v", p, q.From, q.To, q.Via, a.When.Unix()/60, a.ArriveBy, a.Results, a.Products)
	js, ok := s.journeys.get(key)
	if !ok {
		js, err = s.providers[p].Journeys(ctx, q)
		if err != nil {
			return nil, err
		}
		for i := range js {
			for k := range js[i].Legs {
				js[i].Legs[k].TripID = s.alias(js[i].Legs[k].TripID)
			}
			js[i].ID = s.save(p, js[i])
		}
		s.journeys.put(key, js)
	}
	js = inWindow(js, a.When, a.ArriveBy)
	if len(js) > a.Results {
		if a.ArriveBy {
			js = js[len(js)-a.Results:] // the latest arrivals are closest to the time asked for
		} else {
			js = js[:a.Results]
		}
	}
	res.Journeys = withoutStops(js)
	return res, nil
}

// inWindow drops journeys that leave before when (or, arriving by when, arrive after it) — providers like to add one
// before — and sorts by the real departure. If nothing is left the journeys are kept as they are.
func inWindow(js []Journey, when time.Time, arriveBy bool) []Journey {
	var out []Journey
	for _, j := range js {
		first, last := j.Legs[0], j.Legs[len(j.Legs)-1]
		dep, arr := first.Dep, last.Arr
		if first.DepRealtime != nil && first.DepRealtime.After(dep) {
			dep = *first.DepRealtime
		}
		if last.ArrRealtime != nil {
			arr = *last.ArrRealtime
		}
		if !arriveBy && dep.Before(when.Add(-time.Minute)) || arriveBy && arr.After(when.Add(time.Minute)) {
			continue
		}
		out = append(out, j)
	}
	if len(out) == 0 {
		out = slices.Clone(js)
	}
	sort.SliceStable(out, func(i, k int) bool { return realDep(out[i]).Before(realDep(out[k])) })
	return out
}

func realDep(j Journey) time.Time {
	if l := j.Legs[0]; l.DepRealtime != nil {
		return *l.DepRealtime
	}
	return j.Legs[0].Dep
}

// alias gives a long trip id ("db:2|#VN#1#…") a short one ("t-…") the model can copy without mistakes.
func (s *Service) alias(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	short := "t-" + hex.EncodeToString(sum[:])[:10]
	s.trips.put(short, id)
	return short
}

// save keeps a journey (with its stops) for the trip tool; its id is a hash of what it is.
func (s *Service) save(provider string, j Journey) string {
	h := sha256.New()
	fmt.Fprint(h, provider)
	for _, l := range j.Legs {
		fmt.Fprint(h, "|", l.Line, l.From, l.To, l.Dep.Unix(), l.Arr.Unix())
	}
	id := "j-" + hex.EncodeToString(h.Sum(nil))[:10]
	j.ID = id
	s.saved.put(id, j)
	return id
}

func withoutStops(js []Journey) []Journey {
	out := make([]Journey, len(js))
	for i, j := range js {
		j.Legs = slices.Clone(j.Legs)
		for k := range j.Legs {
			j.Legs[k].Stops = nil
		}
		out[i] = j
	}
	return out
}

// TripResult is the answer of trip: a vehicle's run, or a journey with the stops of every leg.
type TripResult struct {
	Trip    *Trip    `json:"trip,omitempty"`
	Journey *Journey `json:"journey,omitempty"`
}

// Trip gives the details of a journey id (from journeys) or of a trip id (from departures or a journey's leg).
func (s *Service) Trip(ctx context.Context, id string) (*TripResult, error) {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "j-") {
		j, ok := s.saved.get(id)
		if !ok {
			return nil, fmt.Errorf("journey %s is unknown or expired (ids from the journeys tool are kept %v): search the journey again", id, SavedTTL)
		}
		return &TripResult{Journey: &j}, nil
	}
	if strings.HasPrefix(id, "t-") {
		long, ok := s.trips.get(id)
		if !ok {
			return nil, fmt.Errorf("trip %s is unknown or expired: ask departures or journeys again", id)
		}
		id = long
	}
	p, raw := s.splitID(id)
	if p == "" {
		return nil, fmt.Errorf("%q is not a trip id: use the tripId of a departure or a leg, or the id of a journey", Clean(id, 80))
	}
	t, err := s.providers[p].Trip(ctx, raw)
	if errors.Is(err, ErrUnsupported) {
		return nil, fmt.Errorf("%s has no trip details; ask journeys for the trip instead, then trip with the journey id", p)
	}
	if err != nil {
		return nil, err
	}
	return &TripResult{Trip: t}, nil
}
