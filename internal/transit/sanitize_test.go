package transit

import (
	"strings"
	"testing"
	"time"
)

func TestClean(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Tübingen  Hbf", "Tübingen Hbf"},
		{"  Mössingen,\tBahnhof\n", "Mössingen, Bahnhof"},
		{"<div>Aufzug <b>außer</b> Betrieb</div>", "Aufzug außer Betrieb"},
		{"A &amp; B &lt;script&gt;alert(1)&lt;/script&gt;", "A & B alert(1)"},
		{"<script>alert(1)</script>Gleis 3", "alert(1) Gleis 3"},
		{"x < y > z", "x z"}, // looks like a tag: removed
		{"a < b", "a b"},
		{"Zero\u200bwidth\u200d join\ufeff", "Zerowidth join"},
		{"bidi \u202eevil\u202c text", "bidi evil text"},
		{"ctrl\x00\x07\x1b[31mred", "ctrl[31mred"},
		{"line\u2028sep\u2029par", "line sep par"},
		{"bad \xff utf8", "bad utf8"},
		{"nbsp\u00a0space", "nbsp space"},
		{"private \ue000 use", "private use"},
		{"", ""},
	} {
		if got := Clean(c.in, 0); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("ä", 300)
	got := Clean(long, 50)
	if r := []rune(got); len(r) != 50 || !strings.HasSuffix(got, "…") {
		t.Errorf("cap: %d runes %q", len(r), got)
	}
}

func TestCleanID(t *testing.T) {
	id := "2|#VN#1#ST#1790795983#PI#0#ZI#266875#ZB#            19306#"
	if got := cleanID(id); got != id {
		t.Errorf("cleanID changed a DB id: %q", got)
	}
	if got := cleanID("de:08416:1006\u200b\x00"); got != "de:08416:1006" {
		t.Errorf("cleanID = %q", got)
	}
	if cleanID(strings.Repeat("x", maxID+1)) != "" {
		t.Error("an over-long id is kept")
	}
}

func TestRemarks(t *testing.T) {
	got := remarks([]string{"<p>Bauarbeiten</p>", "Bauarbeiten", "", "  ", "b", "c", "d"})
	if strings.Join(got, "|") != "Bauarbeiten|b|c" {
		t.Errorf("remarks = %q", got)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 4, 5, 10, 0, 0, time.UTC) // 07:10 in Berlin (CEST)
	for _, c := range []struct{ in, want string }{
		{"", "2026-10-04T07:10:00+02:00"},
		{"now", "2026-10-04T07:10:00+02:00"},
		{"2026-10-05T07:00", "2026-10-05T07:00:00+02:00"},
		{"2026-10-05 07:00:00", "2026-10-05T07:00:00+02:00"},
		{"2026-10-05T05:00:00Z", "2026-10-05T07:00:00+02:00"},
		{"08:15", "2026-10-04T08:15:00+02:00"},
		// winter time after the switch on 25 October
		{"2026-10-26T07:00", "2026-10-26T07:00:00+01:00"},
		// spring: 02:30 does not exist on 29 March 2026; clocks jump to 03:30
		{"2026-03-29T02:30", "2026-03-29T03:30:00+02:00"},
		{"2026-03-29T03:30", "2026-03-29T03:30:00+02:00"},
	} {
		got, err := ParseWhen(c.in, now)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if s := got.Format(time.RFC3339); s != c.want {
			t.Errorf("ParseWhen(%q) = %s, want %s", c.in, s, c.want)
		}
	}
	// autumn: 02:30 happens twice on 25 October 2026; either is a valid reading, but it must be 02:30 local
	got, err := ParseWhen("2026-10-25T02:30", now)
	if err != nil || got.Hour() != 2 || got.Minute() != 30 {
		t.Errorf("ambiguous 02:30 = %v, %v", got, err)
	}
	if _, err := ParseWhen("tomorrow", now); err == nil {
		t.Error("tomorrow accepted (the model resolves it)")
	}
}

func TestTimesAcrossDST(t *testing.T) {
	// DB sends local times without an offset
	summer, _ := dbTime("2026-10-25T01:30:00")
	winter, _ := dbTime("2026-10-25T04:30:00")
	if _, off := summer.Zone(); off != 2*3600 {
		t.Errorf("summer offset %d", off)
	}
	if _, off := winter.Zone(); off != 3600 {
		t.Errorf("winter offset %d", off)
	}
	if d := winter.Sub(summer); d != 4*time.Hour {
		t.Errorf("01:30 CEST → 04:30 CET = %v, want 4h", d)
	}
	// EFA sends UTC
	e := efaTime("2026-10-25T03:30:00Z")
	if e == nil || e.Format("15:04 -0700") != "04:30 +0100" {
		t.Errorf("efaTime = %v", e)
	}
	e = efaTime("2026-10-24T05:00:00Z")
	if e == nil || e.Format("15:04 -0700") != "07:00 +0200" {
		t.Errorf("efaTime = %v", e)
	}
	if d := delay(summer, &winter); d == nil || *d != 240 {
		t.Errorf("delay across the switch = %v", d)
	}
}

func TestCache(t *testing.T) {
	now := time.Date(2026, 10, 4, 7, 0, 0, 0, Berlin)
	c := newCache[int](30*time.Second, 2, func() time.Time { return now })
	c.put("a", 1)
	if v, ok := c.get("a"); !ok || v != 1 {
		t.Fatal("get after put")
	}
	now = now.Add(29 * time.Second)
	if _, ok := c.get("a"); !ok {
		t.Fatal("expired too early")
	}
	now = now.Add(time.Second)
	if _, ok := c.get("a"); ok {
		t.Fatal("not expired after the TTL")
	}
	c.put("a", 1)
	now = now.Add(time.Second)
	c.put("b", 2)
	now = now.Add(time.Second)
	c.put("c", 3) // full: the oldest (a) goes
	if _, ok := c.get("a"); ok {
		t.Error("the oldest was kept")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("the newest is missing")
	}
}
