package transit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/transit/transittest"
)

func fakeService(t *testing.T) (*Service, *transittest.Server, *time.Time) {
	t.Helper()
	srv := transittest.Start()
	t.Cleanup(srv.Close)
	now := transittest.Now
	s := NewService(func() time.Time { return now }, NewDB(srv.URL+"/web/api"), NewEFA("efa", srv.URL+"/nvbw/"),
		NewEFA("naldo", srv.URL+"/naldo/"))
	return s, srv, &now
}

func TestServiceDepartures(t *testing.T) {
	s, srv, _ := fakeService(t)
	ctx := context.Background()
	res, err := s.Departures(ctx, DepartureArgs{Stop: "Tübingen Hbf", When: s.Now(), Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "db" || res.Stop.ID != "db:8000141" || len(res.Departures) != 5 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], `"Tübingen Hbf" → Tübingen Hbf (db:8000141)`) {
		t.Errorf("notes = %q", res.Notes)
	}
	if id := res.Departures[0].TripID; !strings.HasPrefix(id, "t-") || len(id) != 12 {
		t.Errorf("trip id %q is not a short alias", id)
	}
	text := res.Text()
	if !strings.Contains(text, "07:00 +10 (07:10) · RE6a → Stuttgart Hbf · platform 3") {
		t.Errorf("text:\n%s", text)
	}
	// cached: the same question again asks nobody
	n := len(srv.Requests())
	if _, err := s.Departures(ctx, DepartureArgs{Stop: "Tübingen Hbf", When: s.Now(), Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if len(srv.Requests()) != n {
		t.Errorf("not cached: %v", srv.Requests()[n:])
	}
	// the trip of a departure, by its alias
	tr, err := s.Trip(ctx, res.Departures[0].TripID)
	if err != nil || tr.Trip == nil || len(tr.Trip.Stops) == 0 {
		t.Fatalf("trip = %+v, %v", tr, err)
	}
	if srv.Count("/reiseloesung/fahrt?") != 1 || !strings.Contains(srv.Requests()[len(srv.Requests())-1], "journeyId=2%7C%23VN") {
		t.Errorf("trip request: %v", srv.Requests())
	}
}

func TestServiceFallback(t *testing.T) {
	s, srv, _ := fakeService(t)
	srv.Block = true
	ctx := context.Background()
	res, err := s.Departures(ctx, DepartureArgs{Stop: "Tübingen Hbf", When: s.Now(), Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "efa" || !strings.HasPrefix(res.Stop.ID, "efa:") {
		t.Fatalf("provider = %s, stop %s", res.Provider, res.Stop.ID)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "db: the server refused the request (HTTP 403") {
		t.Errorf("notes = %q", res.Notes)
	}
	// asked for db explicitly: the error, no fallback
	_, err = s.Departures(ctx, DepartureArgs{Stop: "Tübingen Hbf", When: s.Now(), Provider: "db"})
	var e *Error
	if !errors.As(err, &e) || !e.Unavailable {
		t.Errorf("err = %v", err)
	}
	fs, err := s.FindStops(ctx, "Mössingen", "", 3)
	if err != nil || fs.Provider != "efa" || len(fs.Stops) != 3 {
		t.Errorf("find_stop fallback = %+v, %v", fs, err)
	}
}

func TestServiceBusesGoToEFA(t *testing.T) {
	s, srv, _ := fakeService(t)
	res, err := s.Departures(context.Background(), DepartureArgs{Stop: "efa:de:08416:10503", When: s.Now(),
		Products: []Product{Bus}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "efa" || srv.Count("/web/api") != 0 {
		t.Errorf("provider %s, requests %v", res.Provider, srv.Requests())
	}
	for _, d := range res.Departures {
		if d.Product != Bus {
			t.Errorf("not a bus: %+v", d)
		}
	}
	res, err = s.Departures(context.Background(), DepartureArgs{Stop: "Tübingen Uni/Neue Aula", When: s.Now(),
		Products: []Product{Bus, Tram}})
	if err != nil || res.Provider != "efa" {
		t.Errorf("bus/tram by name: %+v, %v", res, err)
	}
}

func TestServiceJourneys(t *testing.T) {
	s, srv, _ := fakeService(t)
	ctx := context.Background()
	when := at("2026-10-05 07:00")
	res, err := s.Journeys(ctx, JourneyArgs{From: "Mössingen", To: "Tübingen Hbf", When: when})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "db" || res.From.ID != "db:8004070" || res.To.ID != "db:8000141" || len(res.Journeys) != 4 {
		t.Fatalf("result = %+v", res)
	}
	for _, j := range res.Journeys {
		if !strings.HasPrefix(j.ID, "j-") {
			t.Errorf("journey id %q", j.ID)
		}
		for _, l := range j.Legs {
			if l.Stops != nil {
				t.Error("journeys must not carry the stops")
			}
		}
	}
	if srv.Count("POST /web/api/angebote/fahrplan") != 1 {
		t.Errorf("requests %v", srv.Requests())
	}
	d, err := s.Trip(ctx, res.Journeys[1].ID)
	if err != nil || d.Journey == nil || len(d.Journey.Legs[0].Stops) == 0 {
		t.Fatalf("trip of a journey = %+v, %v", d, err)
	}
	if !strings.Contains(d.Text(), "arr 07:13, dep 07:14 Nehren (pl. 1)") {
		t.Errorf("text:\n%s", d.Text())
	}
	if _, err := s.Trip(ctx, "j-0000000000"); err == nil || !strings.Contains(err.Error(), "unknown or expired") {
		t.Errorf("unknown journey: %v", err)
	}
	if _, err := s.Trip(ctx, "efa:whatever"); err == nil || !strings.Contains(err.Error(), "no trip details") {
		t.Errorf("efa trip: %v", err)
	}

	// EFA: the journey that leaves before 07:00 is dropped
	res, err = s.Journeys(ctx, JourneyArgs{From: "Mössingen", To: "Reutlingen ZOB", When: when, Provider: "naldo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Journeys) != 2 || res.Journeys[0].Legs[0].Dep.Format("15:04") != "07:12" {
		t.Errorf("naldo journeys = %+v", res.Journeys)
	}
	if !strings.Contains(srv.Requests()[len(srv.Requests())-1], "/naldo/XML_TRIP_REQUEST2") {
		t.Errorf("requests %v", srv.Requests())
	}
}

func TestFetchErrors(t *testing.T) {
	srv := transittest.Start()
	defer srv.Close()
	f := newFetcher("x", 0, nil)
	_, err := f.do(context.Background(), "GET", srv.URL+"/html", nil)
	var e *Error
	if !errors.As(err, &e) || strings.Contains(err.Error(), "script") || strings.Contains(err.Error(), "<") {
		t.Errorf("html answer: %v", err)
	}
	_, err = f.do(context.Background(), "GET", srv.URL+"/nothing", nil)
	if !errors.As(err, &e) || e.Unavailable || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.do(ctx, "GET", srv.URL+"/html", nil); err == nil {
		t.Error("cancelled context")
	}
	if _, err := NewService(nil).candidates("vvs", nil); err == nil {
		t.Error("unknown provider accepted")
	}
}
