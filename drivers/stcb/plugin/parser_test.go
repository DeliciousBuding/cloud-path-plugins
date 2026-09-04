package plugin

import (
	"testing"
	"time"
)

func TestParseDumpGolden(t *testing.T) {
	d, ok := ParseDump("S:10801222")
	if !ok {
		t.Fatal("expected valid dump")
	}
	if d.State != 1 || d.Hour != 8 || d.Min != 1 {
		t.Fatalf("state/hour/min = %d/%d/%d", d.State, d.Hour, d.Min)
	}
	if d.Slots != [3]int{2, 2, 2} {
		t.Fatalf("slots = %v", d.Slots)
	}
}

func TestParseDumpRejectsOutOfRange(t *testing.T) {
	// 槽位 6/7/8 越界（合法 0-2）。
	if _, ok := ParseDump("S:12345678"); ok {
		t.Fatal("expected reject")
	}
	// hour 99 越界。
	if _, ok := ParseDump("S:09901000"); ok {
		t.Fatal("expected reject hour 99")
	}
}

func TestParseDumpToleratesDamagedSeparator(t *testing.T) {
	d, ok := ParseDump("S\uFFFD10801222")
	if !ok || d.Hour != 8 {
		t.Fatalf("expected tolerate damaged separator, ok=%v dump=%+v", ok, d)
	}
}

func TestParseSensorGolden(t *testing.T) {
	s, ok := ParseSensor("V:0930552000000000000000010")
	if !ok {
		t.Fatal("expected valid sensor frame")
	}
	if s.Hour != 9 || s.Min != 30 || s.Sec != 55 || s.State != 2 {
		t.Fatalf("clock/state = %02d:%02d:%02d state=%d", s.Hour, s.Min, s.Sec, s.State)
	}
	if s.Hall != 0 || s.Vib != 1 || s.Key != 0 {
		t.Fatalf("hall/vib/key = %d/%d/%d", s.Hall, s.Vib, s.Key)
	}
}

func TestParseSensorRejectsOutOfRange(t *testing.T) {
	// 秒 99 越界。
	if _, ok := ParseSensor("V:0930992000000000000000000"); ok {
		t.Fatal("expected reject sec 99")
	}
}

func TestParseEvent(t *testing.T) {
	cases := map[string]string{
		"REMIND":     "REMIND",
		"TAKEN":      "TAKEN",
		"TAKEN-LATE": "TAKEN-LATE",
		"BOOT":       "BOOT",
		"OK":         "SYNC-OK",
	}
	for in, want := range cases {
		if got, ok := ParseEvent(in); !ok || got != want {
			t.Fatalf("ParseEvent(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := ParseEvent("S:10801222"); ok {
		t.Fatal("dump line must not be treated as event")
	}
}

func TestTempCMidScale(t *testing.T) {
	// Rt=512 → Rt resistor 10K → 25°C。
	got := TempC(512)
	if got < 24.9 || got > 25.1 {
		t.Fatalf("TempC(512) = %f, want ~25.0", got)
	}
}

func TestDriftMinZero(t *testing.T) {
	bj := time.Date(2026, 9, 4, 8, 1, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if got := DriftMin(8, 1, bj); got != 0 {
		t.Fatalf("DriftMin = %f, want 0", got)
	}
}
