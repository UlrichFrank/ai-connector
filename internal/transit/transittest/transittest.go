// Package transittest serves recorded (scrubbed) answers of bahn.de and EFA-BW for tests: a fake of both APIs.
// The recordings are of Sunday 4 October 2026, 07:10 in Tübingen (and journeys for Monday 5 October, 07:00).
package transittest

import (
	"embed"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

//go:embed testdata/*.json
var files embed.FS

// Now is when the recordings were made: 2026-10-04 07:10 in Berlin (05:10 UTC).
var Now = time.Date(2026, 10, 4, 5, 10, 0, 0, time.UTC)

// Fixture is a recorded answer by its file name ("db_abfahrten.json").
func Fixture(name string) []byte {
	b, err := files.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// Server is the fake: DB at URL+"/web/api", EFA at URL+"/nvbw/" and URL+"/naldo/".
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	requests []string
	// Block makes the DB part answer 403 like bahn.de's bot protection.
	Block bool
}

// Start starts the fake.
func Start() *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Requests lists the paths asked for so far ("GET /web/api/reiseloesung/orte").
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Count counts the requests whose line contains sub.
func (s *Server) Count(sub string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(r, sub) {
			n++
		}
	}
	return n
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	block := s.Block
	s.mu.Unlock()
	p := r.URL.Path
	q := r.URL.Query()
	name := ""
	switch {
	case strings.HasPrefix(p, "/web/api/"):
		if block || r.Header.Get("Accept-Encoding") != "identity" {
			http.Error(w, `{"status":"ERROR","code":"OPS_BLOCKED"}`, http.StatusForbidden)
			return
		}
		switch strings.TrimPrefix(p, "/web/api") {
		case "/reiseloesung/orte":
			name = "db_orte.json"
			if !strings.Contains(strings.ToLower(q.Get("suchbegriff")), "ssingen") {
				name = "db_orte_tuebingen"
			}
		case "/reiseloesung/abfahrten":
			name = "db_abfahrten.json"
		case "/reiseloesung/fahrt":
			name = "db_fahrt.json"
		case "/angebote/fahrplan":
			body, _ := io.ReadAll(r.Body)
			name = "db_fahrplan.json"
			if strings.Contains(string(body), "8004070") {
				name = "db_fahrplan_tomorrow.json"
			}
		}
	case strings.HasPrefix(p, "/nvbw/"), strings.HasPrefix(p, "/naldo/"):
		switch p[strings.LastIndex(p, "/")+1:] {
		case "XML_STOPFINDER_REQUEST":
			name = "efa_stopfinder.json"
		case "XML_DM_REQUEST":
			name = "efa_dm_hbf.json"
			if q.Get("name_dm") == "de:08416:10503" {
				name = "efa_dm_aula.json"
			}
		case "XML_TRIP_REQUEST2":
			name = "efa_trip.json"
		}
	case p == "/html":
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>Wartungsarbeiten <script>x()</script></body></html>")
		return
	}
	var body []byte
	switch name {
	case "":
		http.NotFound(w, r)
		return
	case "db_orte_tuebingen":
		body = []byte(`[{"extId":"8000141","id":"A=1@O=Tübingen Hbf@L=8000141@","lat":48.515645,"lon":9.055392,` +
			`"name":"Tübingen Hbf","products":["REGIONAL","BUS"],"type":"ST"},` +
			`{"extId":"8000096","id":"A=1@O=Stuttgart Hbf@L=8000096@","lat":48.78,"lon":9.18,` +
			`"name":"Stuttgart Hbf","products":["ICE","REGIONAL","SBAHN"],"type":"ST"}]`)
		if strings.Contains(strings.ToLower(q.Get("suchbegriff")), "stuttgart") {
			body = []byte(`[{"extId":"8000096","name":"Stuttgart Hbf","products":["ICE"],"type":"ST"}]`)
		}
	default:
		body = Fixture(name)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}
