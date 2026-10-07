// Package transit answers timetable questions (stops, departures, journeys, trips) from public, keyless timetable
// APIs: Deutsche Bahn (bahn.de's web API: all of Germany, trains with real-time delays and platforms) and EFA-BW
// (Baden-Württemberg's EFA by NVBW, which also serves naldo's and VVS's journey planners: buses, trams, regional
// trains). Every string that comes from a provider is sanitized before it leaves the package; all times are in
// Europe/Berlin. cmd/transit-mcp (and "mlxlab mcp serve transit") serve it as an MCP server.
package transit

import (
	"context"
	"time"
	_ "time/tzdata" // Europe/Berlin even without a system zoneinfo
)

// Berlin is the time zone of every time in this package.
var Berlin = mustLoad("Europe/Berlin")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// Product is a kind of transport, normalized across providers.
type Product string

const (
	Train    Product = "train"    // regional and long-distance trains (RE, RB, IC, ICE, …)
	SBahn    Product = "sbahn"    // S-Bahn
	Subway   Product = "subway"   // U-Bahn
	Tram     Product = "tram"     // tram, Stadtbahn
	Bus      Product = "bus"      // city, regional and express buses
	Ferry    Product = "ferry"    //
	OnDemand Product = "ondemand" // Rufbus, AST (must be booked)
	Walk     Product = "walk"     // a footpath between two legs
	Other    Product = "other"
)

// FilterProducts are the values the tools accept in products.
var FilterProducts = []Product{Train, SBahn, Subway, Tram, Bus, Ferry, OnDemand}

// Stop is a station or stop. ID carries the provider ("db:8000141", "efa:de:08416:11000").
type Stop struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Place    string    `json:"place,omitempty"`
	Products []Product `json:"products,omitempty"`
	Lat      float64   `json:"lat,omitempty"`
	Lon      float64   `json:"lon,omitempty"`
	Provider string    `json:"provider"`
}

// Departure is one departure at a stop.
type Departure struct {
	Line      string     `json:"line"`
	Product   Product    `json:"product"`
	Direction string     `json:"direction,omitempty"`
	Planned   time.Time  `json:"planned"`
	Realtime  *time.Time `json:"realtime,omitempty"` // nil: no real-time data
	Delay     *int       `json:"delayMinutes,omitempty"`
	Platform  string     `json:"platform,omitempty"`
	// PlannedPlatform is set when the platform changed.
	PlannedPlatform string   `json:"plannedPlatform,omitempty"`
	Cancelled       bool     `json:"cancelled,omitempty"`
	Remarks         []string `json:"remarks,omitempty"`
	// TripID can be passed to the trip tool ("t-…", DB only).
	TripID string `json:"tripId,omitempty"`
}

// Leg is one part of a journey: a ride or a walk.
type Leg struct {
	Line        string     `json:"line,omitempty"`
	Product     Product    `json:"product"`
	Direction   string     `json:"direction,omitempty"`
	From        string     `json:"from"`
	To          string     `json:"to"`
	Dep         time.Time  `json:"dep"`
	DepRealtime *time.Time `json:"depRealtime,omitempty"`
	DepDelay    *int       `json:"depDelayMinutes,omitempty"`
	Arr         time.Time  `json:"arr"`
	ArrRealtime *time.Time `json:"arrRealtime,omitempty"`
	ArrDelay    *int       `json:"arrDelayMinutes,omitempty"`
	DepPlatform string     `json:"depPlatform,omitempty"`
	ArrPlatform string     `json:"arrPlatform,omitempty"`
	Walking     bool       `json:"walking,omitempty"`
	Cancelled   bool       `json:"cancelled,omitempty"`
	Remarks     []string   `json:"remarks,omitempty"`
	TripID      string     `json:"tripId,omitempty"`
	// Stops are the stops of the ride (only in trip details).
	Stops []StopTime `json:"stops,omitempty"`
}

// Journey is a connection from one stop to another.
type Journey struct {
	// ID can be passed to the trip tool for the details (intermediate stops) of this journey.
	ID        string   `json:"id,omitempty"`
	Legs      []Leg    `json:"legs"`
	Duration  int      `json:"durationMinutes"`
	Transfers int      `json:"transfers"`
	Remarks   []string `json:"remarks,omitempty"`
}

// StopTime is a stop of a trip.
type StopTime struct {
	Name        string     `json:"name"`
	Arr         *time.Time `json:"arr,omitempty"`
	ArrRealtime *time.Time `json:"arrRealtime,omitempty"`
	Dep         *time.Time `json:"dep,omitempty"`
	DepRealtime *time.Time `json:"depRealtime,omitempty"`
	Platform    string     `json:"platform,omitempty"`
	Cancelled   bool       `json:"cancelled,omitempty"`
}

// Trip is the run of one vehicle.
type Trip struct {
	ID        string     `json:"id"`
	Line      string     `json:"line,omitempty"`
	Product   Product    `json:"product,omitempty"`
	Direction string     `json:"direction,omitempty"`
	Stops     []StopTime `json:"stops"`
	Cancelled bool       `json:"cancelled,omitempty"`
	Remarks   []string   `json:"remarks,omitempty"`
}

// DepartureQuery asks a provider for departures. StopID is the provider's own id (without the prefix).
type DepartureQuery struct {
	StopID   string
	When     time.Time
	Duration time.Duration
	Products []Product // empty: all
	Limit    int
}

// JourneyQuery asks a provider for journeys; the ids are the provider's own.
type JourneyQuery struct {
	From, To, Via string
	When          time.Time
	ArriveBy      bool
	Results       int
	Products      []Product // empty: all
}

// Provider is a timetable source.
type Provider interface {
	// Name is the provider's name ("db", "efa", "naldo"), also the prefix of its ids.
	Name() string
	FindStops(ctx context.Context, query string, limit int) ([]Stop, error)
	Departures(ctx context.Context, q DepartureQuery) ([]Departure, error)
	Journeys(ctx context.Context, q JourneyQuery) ([]Journey, error)
	// Trip gives the stops of one vehicle's run; ErrUnsupported when the provider cannot.
	Trip(ctx context.Context, id string) (*Trip, error)
}

// delay is realtime-planned in whole minutes (nil without real-time data).
func delay(planned time.Time, rt *time.Time) *int {
	if rt == nil || planned.IsZero() {
		return nil
	}
	d := int(rt.Sub(planned).Round(time.Minute) / time.Minute)
	return &d
}

func wants(products []Product, p Product) bool {
	if len(products) == 0 || p == Walk {
		return true
	}
	for _, x := range products {
		if x == p {
			return true
		}
	}
	return false
}
