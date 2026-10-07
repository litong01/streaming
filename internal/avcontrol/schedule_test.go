package avcontrol

import (
	"testing"
	"time"
)

func TestScheduleCatchUpOnAndIgnoresAPastOff(t *testing.T) {
	file := loadFixture(t)
	loc := file.Location()
	sunday := time.Date(2026, 10, 11, 10, 0, 0, 0, loc)
	if sunday.Weekday() != time.Sunday {
		t.Fatalf("weekday %s", sunday.Weekday())
	}

	inside := decide(agenda{
		Now:   sunday,
		Boot:  time.Date(2026, 10, 11, 9, 0, 0, 0, loc),
		File:  file,
		Fired: map[string]bool{},
		Phase: phaseOff,
	})
	if !inside.StartOn || inside.StartOff {
		t.Fatalf("inside window: on %t off %t", inside.StartOn, inside.StartOff)
	}

	late := decide(agenda{
		Now:   time.Date(2026, 10, 11, 15, 0, 0, 0, loc),
		Boot:  time.Date(2026, 10, 11, 15, 0, 0, 0, loc),
		File:  file,
		Fired: map[string]bool{},
		Phase: phaseOn,
	})
	if late.StartOn || late.StartOff {
		t.Fatalf("past off was not consumed: on %t off %t marks %v", late.StartOn, late.StartOff, late.Mark)
	}

	running := decide(agenda{
		Now:   time.Date(2026, 10, 11, 14, 0, 0, 0, loc),
		Boot:  time.Date(2026, 10, 11, 13, 0, 0, 0, loc),
		File:  file,
		Fired: map[string]bool{},
		Phase: phaseOn,
	})
	if !running.StartOff || running.StartOn {
		t.Fatalf("scheduled off: on %t off %t", running.StartOn, running.StartOff)
	}
}

func TestDelayReplacesTheDisplayedOff(t *testing.T) {
	file := loadFixture(t)
	loc := file.Location()
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, loc)
	pushed := time.Date(2026, 10, 11, 15, 0, 0, 0, loc)
	out := decide(agenda{
		Now:    now,
		Boot:   now.Add(-time.Hour),
		File:   file,
		Fired:  map[string]bool{},
		Pushed: &pushed,
		Phase:  phaseOn,
	})
	if !out.Delayed || !out.NextOff.Equal(pushed) {
		t.Fatalf("next off %s delayed %t", out.NextOff, out.Delayed)
	}
	if out.StartOff {
		t.Fatal("delayed off fired early")
	}

	due := decide(agenda{
		Now:    pushed,
		Boot:   now.Add(-time.Hour),
		File:   file,
		Fired:  map[string]bool{},
		Pushed: &pushed,
		Phase:  phaseOn,
	})
	if !due.StartOff || !due.ClearPush {
		t.Fatalf("due off %t clear %t", due.StartOff, due.ClearPush)
	}
}

func TestNextOffSkipsAStaleTime(t *testing.T) {
	file := loadFixture(t)
	loc := file.Location()
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, loc) // Sunday, after the 2:00 off
	next, delayed := nextOff(now, file, nil)
	if delayed || next.Hour() != 17 || next.Day() != 11 {
		t.Fatalf("next %s delayed %t", next, delayed)
	}
}

func loadFixture(t *testing.T) *File {
	t.Helper()
	file, err := Load(writeFixture(t, fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	return file
}
