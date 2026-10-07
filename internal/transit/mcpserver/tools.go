package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/transit"
)

// Instructions are the server's hints for the model.
const Instructions = `Public transport timetables for Germany, with real-time delays. Home area: Tübingen, Mössingen, Reutlingen, Hechingen (naldo), Stuttgart (VVS).
- Times are Europe/Berlin local time. Resolve "tomorrow morning" etc. yourself from the current date and pass "when" like 2026-10-05T07:00.
- Stop names work everywhere ("Tübingen Hbf", "Mössingen", "Tübingen Uni/Neue Aula"); the answer says which stop was chosen.
- Providers: db = Deutsche Bahn (all trains in Germany, also most buses; real-time, platforms, cancellations); efa = EFA-BW (all of Baden-Württemberg: naldo and VVS buses, trams, Stadtbahn and regional trains with real-time); naldo = the same EFA-BW data as naldo.de's journey planner. Default: db, and efa for bus/tram-only queries or when db fails or finds nothing.
- "+3" after a time = 3 minutes late, "+0" = on time, no sign = no real-time data yet. CANCELLED marks cancelled trips.`

var annotations = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true,
	"openWorldHint": true}

func productsSchema() map[string]any {
	var enum []string
	for _, p := range transit.FilterProducts {
		enum = append(enum, string(p))
	}
	return map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": enum},
		"description": "Only these kinds of transport (default: all). train = regional and long-distance trains (RE, RB, IC, ICE); sbahn = S-Bahn; bus; tram = tram/Stadtbahn; subway = U-Bahn; ondemand = Rufbus/AST."}
}

func providerSchema() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"db", "efa", "naldo"},
		"description": "Timetable source (default: automatic). db = Deutsche Bahn, all of Germany, best for trains; efa = EFA-BW for Baden-Württemberg buses/trams (naldo: Tübingen, Reutlingen, Mössingen, Hechingen, Balingen, Sigmaringen; VVS: Stuttgart); naldo = the same EFA-BW data as naldo's planner."}
}

const whenDesc = `When: "now" (default), or a local Europe/Berlin date and time like "2026-10-05T07:00" (or RFC 3339 with offset). Work out "tomorrow", "Monday morning" etc. from the current date yourself.`

// tool is one tool: its listing and its handler.
type tool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	run         func(ctx context.Context, s *transit.Service, args json.RawMessage) (text string, structured any, err error)
}

func (t tool) listing() map[string]any {
	return map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.Schema,
		"annotations": annotations}
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func tools() []tool {
	return []tool{
		{
			Name:  "find_stop",
			Title: "Find a stop",
			Description: "Find train stations and bus/tram stops by name, with their ids. Use it when a name is ambiguous or to get an id; " +
				"departures and journeys also accept plain names. Covers all of Germany (db) and Baden-Württemberg's buses and trams (efa/naldo: e.g. \"Tübingen Uni/Neue Aula\", \"Reutlingen ZOB\").",
			Schema: object(map[string]any{
				"query":    map[string]any{"type": "string", "description": "Stop name, e.g. \"Mössingen\", \"Tübingen Hbf\", \"Reutlingen ZOB\"."},
				"provider": providerSchema(),
				"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "description": "Number of matches (default 5)."},
			}, "query"),
			run: runFindStop,
		},
		{
			Name:  "departures",
			Title: "Departures at a stop",
			Description: "Departure board of a station or stop: line, direction, planned time, delay (+N minutes), platform, cancellations. " +
				"Use for \"next trains from X\", \"when does the bus leave\", \"is the 7:42 late?\" (look for its line and planned time). " +
				"For buses and trams in Tübingen/Reutlingen/Stuttgart pass products [\"bus\"] or [\"tram\"] (uses EFA-BW).",
			Schema: object(map[string]any{
				"stop":             map[string]any{"type": "string", "description": "Stop name (\"Tübingen Hbf\") or an id from find_stop (\"db:8000141\", \"efa:de:08416:10503\")."},
				"when":             map[string]any{"type": "string", "description": whenDesc},
				"duration_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": 720, "description": "Length of the time window in minutes (default 60)."},
				"products":         productsSchema(),
				"limit":            map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "description": "Maximum number of departures (default 15)."},
				"provider":         providerSchema(),
			}, "stop"),
			run: runDepartures,
		},
		{
			Name:  "journeys",
			Title: "Plan a journey",
			Description: "Connections from one stop to another (trains, buses, trams, walks), with transfers, platforms and real-time delays. " +
				"Use for \"how do I get from X to Y\", \"when is there a train from Mössingen to Tübingen tomorrow morning\". " +
				"Names are resolved automatically and the answer says which stops were used. Each journey has an id for the trip tool (all intermediate stops).",
			Schema: object(map[string]any{
				"from":      map[string]any{"type": "string", "description": "Start: stop name or id."},
				"to":        map[string]any{"type": "string", "description": "Destination: stop name or id."},
				"via":       map[string]any{"type": "string", "description": "Optional stop to pass through."},
				"when":      map[string]any{"type": "string", "description": whenDesc},
				"arrive_by": map[string]any{"type": "boolean", "description": "true: \"when\" is the latest arrival time instead of the earliest departure."},
				"results":   map[string]any{"type": "integer", "minimum": 1, "maximum": 6, "description": "Number of journeys (default 4)."},
				"products":  productsSchema(),
				"provider":  providerSchema(),
			}, "from", "to"),
			run: runJourneys,
		},
		{
			Name:  "trip",
			Title: "Trip details",
			Description: "Details of one journey or one train/bus run: every stop with times, delays and platforms. " +
				"Pass a journey id from journeys (\"j-…\") or a tripId from departures or a journey leg (\"db:…\", DB only).",
			Schema: object(map[string]any{
				"id": map[string]any{"type": "string", "description": "Journey id (\"j-…\") or trip id (\"db:…\")."},
			}, "id"),
			run: runTrip,
		},
	}
}

// decode reads the arguments strictly (an unknown argument is an error the model can fix).
func decode(args json.RawMessage, v any) error {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

func products(in []string) ([]transit.Product, error) {
	var out []transit.Product
	for _, s := range in {
		p := transit.Product(strings.ToLower(strings.TrimSpace(s)))
		switch p {
		case "trains", "rail", "zug":
			p = transit.Train
		case "s-bahn":
			p = transit.SBahn
		case "buses":
			p = transit.Bus
		case "trams", "stadtbahn", "lightrail":
			p = transit.Tram
		case "u-bahn", "metro":
			p = transit.Subway
		}
		ok := false
		for _, f := range transit.FilterProducts {
			ok = ok || f == p
		}
		if !ok {
			return nil, fmt.Errorf("unknown product %q (use train, sbahn, bus, tram, subway, ferry, ondemand)", s)
		}
		out = append(out, p)
	}
	return out, nil
}

func runFindStop(ctx context.Context, s *transit.Service, raw json.RawMessage) (string, any, error) {
	var a struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
		Limit    int    `json:"limit"`
	}
	if err := decode(raw, &a); err != nil {
		return "", nil, err
	}
	res, err := s.FindStops(ctx, a.Query, a.Provider, a.Limit)
	if err != nil {
		return "", nil, err
	}
	return res.Text(), res, nil
}

func runDepartures(ctx context.Context, s *transit.Service, raw json.RawMessage) (string, any, error) {
	var a struct {
		Stop     string   `json:"stop"`
		When     string   `json:"when"`
		Duration int      `json:"duration_minutes"`
		Products []string `json:"products"`
		Limit    int      `json:"limit"`
		Provider string   `json:"provider"`
	}
	if err := decode(raw, &a); err != nil {
		return "", nil, err
	}
	when, err := transit.ParseWhen(a.When, s.Now())
	if err != nil {
		return "", nil, err
	}
	ps, err := products(a.Products)
	if err != nil {
		return "", nil, err
	}
	if a.Duration < 0 || a.Duration > 720 {
		return "", nil, errors.New("duration_minutes: 1 to 720")
	}
	if a.Limit < 0 || a.Limit > 60 {
		return "", nil, errors.New("limit: 1 to 60")
	}
	res, err := s.Departures(ctx, transit.DepartureArgs{Stop: a.Stop, When: when,
		Duration: time.Duration(a.Duration) * time.Minute, Products: ps, Limit: a.Limit, Provider: a.Provider})
	if err != nil {
		return "", nil, err
	}
	return res.Text(), res, nil
}

func runJourneys(ctx context.Context, s *transit.Service, raw json.RawMessage) (string, any, error) {
	var a struct {
		From     string   `json:"from"`
		To       string   `json:"to"`
		Via      string   `json:"via"`
		When     string   `json:"when"`
		ArriveBy bool     `json:"arrive_by"`
		Results  int      `json:"results"`
		Products []string `json:"products"`
		Provider string   `json:"provider"`
	}
	if err := decode(raw, &a); err != nil {
		return "", nil, err
	}
	when, err := transit.ParseWhen(a.When, s.Now())
	if err != nil {
		return "", nil, err
	}
	ps, err := products(a.Products)
	if err != nil {
		return "", nil, err
	}
	res, err := s.Journeys(ctx, transit.JourneyArgs{From: a.From, To: a.To, Via: a.Via, When: when,
		ArriveBy: a.ArriveBy, Results: a.Results, Products: ps, Provider: a.Provider})
	if err != nil {
		return "", nil, err
	}
	return res.Text(), res, nil
}

func runTrip(ctx context.Context, s *transit.Service, raw json.RawMessage) (string, any, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(raw, &a); err != nil {
		return "", nil, err
	}
	res, err := s.Trip(ctx, a.ID)
	if err != nil {
		return "", nil, err
	}
	return res.Text(), res, nil
}
