package transit

import (
	"fmt"
	"strings"
	"time"
)

// ParseWhen reads a "when" argument: "" or "now" is now; RFC 3339 with an offset is that instant; a local date and
// time without an offset ("2026-10-05T07:00", "2026-10-05 07:00:00") is Europe/Berlin; a bare time ("07:00") is
// today in Europe/Berlin. In the autumn hour that happens twice the first (summer time) one is taken; a time that
// does not exist in spring (02:30) moves forward by an hour, as clocks do.
func ParseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "now", "jetzt":
		return now.In(Berlin), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(Berlin), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, Berlin); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"15:04", "15:04:05", "15.04"} {
		if t, err := time.Parse(layout, s); err == nil {
			n := now.In(Berlin)
			return time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), t.Second(), 0, Berlin), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, Berlin); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("when %q: use \"now\", a local time like 2026-10-05T07:00 (Europe/Berlin) or RFC 3339", s)
}
