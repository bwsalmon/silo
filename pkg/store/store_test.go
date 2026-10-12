package store

import (
	"testing"
	"time"
)

func TestParseSpec(t *testing.T) {
	for in, want := range map[string]Spec{
		"postgres":              {Backend: "postgres"},
		"sqlite:/var/silo.db":   {Backend: "sqlite", Path: "/var/silo.db"},
		"sqlite:rel/dir/s.db":   {Backend: "sqlite", Path: "rel/dir/s.db"},
		"sqlite:C:/odd:path.db": {Backend: "sqlite", Path: "C:/odd:path.db"},
	} {
		got, err := ParseSpec(in)
		if err != nil || got != want {
			t.Errorf("ParseSpec(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "sqlite", "sqlite:", "postgres:x", "mysql:x"} {
		if got, err := ParseSpec(bad); err == nil {
			t.Errorf("ParseSpec(%q) = %+v, want an error", bad, got)
		}
	}
}

func TestTimestamps(t *testing.T) {
	at := time.Date(2026, 10, 5, 7, 12, 0, 345_000_000, time.FixedZone("x", 3600))
	ms := Millis(at)
	if ms != at.UnixMilli() {
		t.Errorf("Millis = %d", ms)
	}
	back := Time(ms)
	if !back.Equal(at) || back.Location() != time.UTC {
		t.Errorf("Time(Millis(t)) = %v, want %v in UTC", back, at)
	}
}
