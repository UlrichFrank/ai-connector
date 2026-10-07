package transit

import (
	"fmt"
	"strings"
	"time"
)

var weekdays = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func day(t time.Time) string {
	t = t.In(Berlin)
	return fmt.Sprintf("%s %d %s", weekdays[t.Weekday()], t.Day(), t.Month().String()[:3])
}

func hm(t time.Time) string { return t.In(Berlin).Format("15:04") }

// clock is "07:11", "07:11 +3" (delayed, with the real-time 07:14 in brackets when more than a few minutes), or
// "07:11 on time" never: a missing delay means no real-time data, "+0" means on time.
func clock(planned time.Time, rt *time.Time, d *int) string {
	s := hm(planned)
	if d == nil {
		return s
	}
	switch {
	case *d > 0:
		s += fmt.Sprintf(" +%d", *d)
	case *d < 0:
		s += fmt.Sprintf(" %d", *d)
	default:
		s += " +0"
	}
	if rt != nil && (*d >= 5 || *d < 0) {
		s += " (" + hm(*rt) + ")"
	}
	return s
}

func providerName(p string) string {
	switch p {
	case "db":
		return "DB (bahn.de)"
	case "efa":
		return "EFA-BW"
	case "naldo":
		return "naldo (EFA-BW)"
	}
	return p
}

func writeNotes(b *strings.Builder, notes []string) {
	for _, n := range notes {
		fmt.Fprintf(b, "Note: %s\n", n)
	}
}

// Text of find_stop.
func (r *FindStopsResult) Text() string {
	var b strings.Builder
	writeNotes(&b, r.Notes)
	if len(r.Stops) == 0 {
		fmt.Fprintf(&b, "No stop found for %q.\n", r.Query)
		return b.String()
	}
	fmt.Fprintf(&b, "Stops for %q (source: %s):\n", r.Query, providerName(r.Provider))
	for _, s := range r.Stops {
		line := fmt.Sprintf("- %s [id %s]", s.Name, s.ID)
		if s.Place != "" && !strings.Contains(s.Name, s.Place) {
			line += " in " + s.Place
		}
		if len(s.Products) > 0 {
			var ps []string
			for _, p := range s.Products {
				ps = append(ps, string(p))
			}
			line += " — " + strings.Join(ps, ", ")
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// Text of departures: one line per departure.
func (r *DeparturesResult) Text() string {
	var b strings.Builder
	writeNotes(&b, r.Notes)
	fmt.Fprintf(&b, "Departures at %s [id %s], %s from %s, next %d min (source: %s; +N = minutes late, no sign = no real-time data):\n",
		r.Stop.Name, r.Stop.ID, day(r.When), hm(r.When), r.Minutes, providerName(r.Provider))
	if len(r.Departures) == 0 {
		b.WriteString("(none in this window)\n")
	}
	today := r.When.In(Berlin).YearDay()
	for _, d := range r.Departures {
		var parts []string
		t := clock(d.Planned, d.Realtime, d.Delay)
		if d.Planned.In(Berlin).YearDay() != today {
			t = day(d.Planned) + " " + t
		}
		parts = append(parts, t)
		if d.Cancelled {
			parts = append(parts, "CANCELLED")
		}
		dir := d.Line
		if d.Direction != "" {
			dir += " → " + d.Direction
		}
		parts = append(parts, dir)
		if d.Platform != "" {
			p := "platform " + d.Platform
			if d.PlannedPlatform != "" {
				p += " (changed from " + d.PlannedPlatform + ")"
			}
			parts = append(parts, p)
		}
		if len(d.Remarks) > 0 {
			parts = append(parts, short(d.Remarks[0], 100))
		}
		if d.TripID != "" {
			parts = append(parts, "trip "+d.TripID)
		}
		b.WriteString(strings.Join(parts, " · ") + "\n")
	}
	return b.String()
}

func short(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Text of journeys.
func (r *JourneysResult) Text() string {
	var b strings.Builder
	writeNotes(&b, r.Notes)
	mode := "departing from"
	if r.ArriveBy {
		mode = "arriving by"
	}
	fmt.Fprintf(&b, "Journeys %s [id %s] → %s [id %s]", r.From.Name, r.From.ID, r.To.Name, r.To.ID)
	if r.Via != nil {
		fmt.Fprintf(&b, " via %s", r.Via.Name)
	}
	fmt.Fprintf(&b, ", %s %s %s (source: %s; +N = minutes late):\n", mode, day(r.When), hm(r.When), providerName(r.Provider))
	if len(r.Journeys) == 0 {
		b.WriteString("(no journey found)\n")
	}
	for i, j := range r.Journeys {
		first, last := j.Legs[0], j.Legs[len(j.Legs)-1]
		fmt.Fprintf(&b, "%d) %s → %s (%d min, %s)", i+1, clock(first.Dep, first.DepRealtime, first.DepDelay),
			clock(last.Arr, last.ArrRealtime, last.ArrDelay), j.Duration, plural(j.Transfers, "transfer", "transfers"))
		if day(first.Dep) != day(r.When) {
			fmt.Fprintf(&b, " on %s", day(first.Dep))
		}
		for _, l := range j.Legs {
			if l.Cancelled {
				b.WriteString(" · PART CANCELLED")
				break
			}
		}
		fmt.Fprintf(&b, " [id %s]\n", j.ID)
		for _, l := range j.Legs {
			b.WriteString("   " + legLine(l) + "\n")
		}
		for _, n := range j.Remarks {
			b.WriteString("   ! " + short(n, 160) + "\n")
		}
	}
	return b.String()
}

func legLine(l Leg) string {
	if l.Walking {
		m := int(l.Arr.Sub(l.Dep).Round(time.Minute) / time.Minute)
		if l.From == l.To || l.From == "" {
			return fmt.Sprintf("walk %d min to %s", m, l.To)
		}
		return fmt.Sprintf("walk %d min: %s → %s", m, l.From, l.To)
	}
	s := fmt.Sprintf("%s %s", clock(l.Dep, l.DepRealtime, l.DepDelay), l.From)
	if l.DepPlatform != "" {
		s += " (pl. " + l.DepPlatform + ")"
	}
	s += fmt.Sprintf(" → %s %s", clock(l.Arr, l.ArrRealtime, l.ArrDelay), l.To)
	if l.ArrPlatform != "" {
		s += " (pl. " + l.ArrPlatform + ")"
	}
	s += " · " + l.Line
	if l.Direction != "" {
		s += " towards " + l.Direction
	}
	if l.Cancelled {
		s += " · CANCELLED"
	}
	if len(l.Remarks) > 0 {
		s += " · " + short(l.Remarks[0], 120)
	}
	return s
}

// Text of trip.
func (r *TripResult) Text() string {
	var b strings.Builder
	if j := r.Journey; j != nil {
		first, last := j.Legs[0], j.Legs[len(j.Legs)-1]
		fmt.Fprintf(&b, "Journey %s: %s %s → %s %s (%d min, %s)\n", j.ID, day(first.Dep),
			clock(first.Dep, first.DepRealtime, first.DepDelay), clock(last.Arr, last.ArrRealtime, last.ArrDelay),
			last.To, j.Duration, plural(j.Transfers, "transfer", "transfers"))
		for _, l := range j.Legs {
			b.WriteString("- " + legLine(l) + "\n")
			for _, st := range l.Stops {
				b.WriteString("    " + stopLine(st) + "\n")
			}
			for _, n := range l.Remarks[min(1, len(l.Remarks)):] {
				b.WriteString("    ! " + n + "\n")
			}
			if l.TripID != "" {
				fmt.Fprintf(&b, "    (trip %s)\n", l.TripID)
			}
		}
		for _, n := range j.Remarks {
			b.WriteString("! " + n + "\n")
		}
		return b.String()
	}
	t := r.Trip
	if t == nil {
		return "(nothing)\n"
	}
	fmt.Fprintf(&b, "Trip %s", t.Line)
	if t.Direction != "" {
		fmt.Fprintf(&b, " towards %s", t.Direction)
	}
	if len(t.Stops) > 0 {
		if st := t.Stops[0]; st.Dep != nil {
			fmt.Fprintf(&b, ", %s", day(*st.Dep))
		}
	}
	if t.Cancelled {
		b.WriteString(" · CANCELLED")
	}
	b.WriteString(":\n")
	for _, st := range t.Stops {
		b.WriteString("  " + stopLine(st) + "\n")
	}
	for _, n := range t.Remarks {
		b.WriteString("! " + n + "\n")
	}
	return b.String()
}

func stopLine(st StopTime) string {
	var times []string
	if st.Arr != nil {
		times = append(times, "arr "+clock(*st.Arr, st.ArrRealtime, delay(*st.Arr, st.ArrRealtime)))
	}
	if st.Dep != nil {
		times = append(times, "dep "+clock(*st.Dep, st.DepRealtime, delay(*st.Dep, st.DepRealtime)))
	}
	s := strings.Join(times, ", ")
	if s != "" {
		s += " "
	}
	s += st.Name
	if st.Platform != "" {
		s += " (pl. " + st.Platform + ")"
	}
	if st.Cancelled {
		s += " · CANCELLED (no stop)"
	}
	return s
}
