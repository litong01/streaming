package avcontrol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

var errNoReply = errors.New("no reply")

type fakeCmd struct {
	mu     sync.Mutex
	sets   []setCall
	states map[string]bool
	stuck  map[string]bool
	silent bool
	err    error
}

type setCall struct {
	id string
	on bool
}

func (f *fakeCmd) Set(_ context.Context, sw Switch, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sets = append(f.sets, setCall{sw.ID, on})
	if f.states == nil {
		f.states = map[string]bool{}
	}
	if _, held := f.stuck[sw.ID]; !held {
		f.states[sw.ID] = on
	}
	return nil
}

func (f *fakeCmd) States(_ context.Context, switches []Switch) map[string]Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]Reading{}
	for _, sw := range switches {
		if f.silent {
			out[sw.ID] = Reading{Err: errNoReply}
			continue
		}
		if on, ok := f.states[sw.ID]; ok {
			out[sw.ID] = Reading{On: on}
		} else {
			out[sw.ID] = Reading{Err: errNoReply}
		}
	}
	return out
}

func (f *fakeCmd) Use(*File) {}

func (f *fakeCmd) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setCall(nil), f.sets...)
}

func TestSequenceTurnsSwitchesOnThenOffInReverse(t *testing.T) {
	var waits []time.Duration
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, mustZone(t))
	cmd := &fakeCmd{}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: cmd,
		Now:      func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	})
	if err := c.TurnOn(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	got := cmd.calls()
	if len(got) != 2 || got[0].id != "a" || !got[0].on || got[1].id != "b" || !got[1].on {
		t.Fatalf("on sequence %#v", got)
	}
	if len(waits) != 1 || waits[0] != 2*time.Second {
		t.Fatalf("waits %#v", waits)
	}
	if c.Status().Phase != phaseOn {
		t.Fatalf("phase %s", c.Status().Phase)
	}

	waits = nil
	cmd.sets = nil
	now = now.Add(manualPause)
	if err := c.TurnOff(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	got = cmd.calls()
	if len(got) != 2 || got[0].id != "b" || got[0].on || got[1].id != "a" || got[1].on {
		t.Fatalf("off sequence %#v", got)
	}
	if c.Status().Phase != phaseOff {
		t.Fatalf("phase after off %s", c.Status().Phase)
	}
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Fatalf("off waits %#v", waits)
	}
}

func TestStatusFollowsReportedSwitchState(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, mustZone(t))
	cmd := &fakeCmd{
		states: map[string]bool{"a": true, "b": true},
		stuck:  map[string]bool{"b": true},
	}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: cmd,
		Now:      func() time.Time { return now },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err := c.TurnOff(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	status := c.Status()
	if status.Phase != phasePartial || status.Error != "" {
		t.Fatalf("status %#v", status)
	}

	cmd.silent = true
	now = now.Add(manualPause)
	if err := c.TurnOn(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	status = c.Status()
	if status.Phase != phaseError || !hasError(status.Errors, "Alpha: could not read status: no reply") || !hasError(status.Errors, "Beta: could not read status: no reply") {
		t.Fatalf("unread status %#v", status)
	}
}

func TestCommandFailureStaysUntilThatSwitchSucceeds(t *testing.T) {
	cmd := &fakeCmd{
		states: map[string]bool{"a": false, "b": false},
		err:    errors.New("connection refused"),
	}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: cmd,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err := c.TurnOn(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	status := c.Status()
	if !hasError(status.Errors, "Alpha: command failed: connection refused") {
		t.Fatalf("command error missing %#v", status.Errors)
	}

	cmd.err = nil
	if err := c.TurnOn(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	status = c.Status()
	if hasError(status.Errors, "command failed") {
		t.Fatalf("command error stayed %#v", status.Errors)
	}
	if status.Phase != phaseOn {
		t.Fatalf("phase %s", status.Phase)
	}
}

func hasError(errors []string, want string) bool {
	for _, err := range errors {
		if err == want || strings.Contains(err, want) {
			return true
		}
	}
	return false
}

func TestButtonWaitHoldsTheOppositeButton(t *testing.T) {
	loc := mustZone(t)
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, loc)
	cmd := &fakeCmd{}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: cmd,
		Now:      func() time.Time { return now },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err := c.TurnOn(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if err := c.TurnOff(); !errors.Is(err, ErrButtonLocked) {
		t.Fatalf("off during the wait: %v", err)
	}
	if len(cmd.calls()) != 2 {
		t.Fatalf("off ran during the wait %#v", cmd.calls())
	}
	status := c.Status()
	if status.OffLockedUntil == "" || !strings.Contains(status.Status, "Off waits until 10:05 AM") {
		t.Fatalf("status %#v", status)
	}

	now = now.Add(manualPause)
	if err := c.TurnOff(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if err := c.TurnOn(); !errors.Is(err, ErrButtonLocked) {
		t.Fatalf("on during the wait: %v", err)
	}
	status = c.Status()
	if status.OnLockedUntil == "" || !strings.Contains(status.Status, "On waits until 10:10 AM") {
		t.Fatalf("after off %#v", status)
	}
}

func TestScheduleStillRunsDuringTheButtonWait(t *testing.T) {
	loc := mustZone(t)
	now := time.Date(2026, 10, 11, 13, 59, 0, 0, loc)
	cmd := &fakeCmd{}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: cmd,
		Now:      func() time.Time { return now },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	c.booted = now.Add(-time.Hour)
	if err := c.TurnOn(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	now = time.Date(2026, 10, 11, 14, 0, 0, 0, loc)
	c.reload()
	c.tick()
	waitIdle(t, c)
	got := cmd.calls()
	if len(got) != 4 || got[2].on || got[3].on {
		t.Fatalf("scheduled off during the wait %#v", got)
	}
}

func TestDelayMovesTheDisplayedOff(t *testing.T) {
	loc := mustZone(t)
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, loc)
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: &fakeCmd{states: map[string]bool{"a": true, "b": true}},
		Now:      func() time.Time { return now },
	})
	c.phase = phaseOn
	status := c.Status()
	if !status.Configured || status.Status != "On · Off at 2:00 PM" {
		t.Fatalf("status %#v", status)
	}
	if err := c.DelayOff(); err != nil {
		t.Fatal(err)
	}
	status = c.Status()
	if !status.Delayed || status.Status != "On · Off at 3:00 PM, delayed" {
		t.Fatalf("delayed %#v", status)
	}
}

func TestScheduledOffHoldsWhileStreaming(t *testing.T) {
	loc := mustZone(t)
	boot := time.Date(2026, 10, 11, 13, 0, 0, 0, loc)
	now := time.Date(2026, 10, 11, 14, 0, 0, 0, loc)
	streaming := true
	cmd := &fakeCmd{}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands:     cmd,
		Now:          func() time.Time { return now },
		StreamActive: func() bool { return streaming },
		Sleep:        func(context.Context, time.Duration) error { return nil },
	})
	c.booted = boot
	c.phase = phaseOn
	c.reload()
	c.tick()
	if len(cmd.calls()) != 0 || !c.Status().Held {
		t.Fatalf("held %t calls %#v status %#v", c.Status().Held, cmd.calls(), c.Status())
	}

	streaming = false
	now = now.Add(6 * time.Minute)
	c.tick()
	waitIdle(t, c)
	got := cmd.calls()
	if len(got) != 2 || got[0].on || got[1].on {
		t.Fatalf("off after the hold %#v", got)
	}
}

func TestManualOffIgnoresTheStreamGuard(t *testing.T) {
	cmd := &fakeCmd{}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands:     cmd,
		StreamActive: func() bool { return true },
		Sleep:        func(context.Context, time.Duration) error { return nil },
	})
	if err := c.TurnOff(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if len(cmd.calls()) != 2 {
		t.Fatalf("manual off did not run %#v", cmd.calls())
	}
}

func TestScheduledOnWhileInsideTheWindow(t *testing.T) {
	loc := mustZone(t)
	boot := time.Date(2026, 10, 11, 9, 0, 0, 0, loc)
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, loc)
	cmd := &fakeCmd{}
	c := New(writeFixture(t, fixtureYAML), Options{
		Commands: cmd,
		Now:      func() time.Time { return now },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	c.booted = boot
	c.reload()
	c.tick()
	waitIdle(t, c)
	got := cmd.calls()
	if len(got) != 2 || !got[0].on || !got[1].on {
		t.Fatalf("catch-up on %#v", got)
	}
}

func waitIdle(t *testing.T, c *Controller) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !c.isBusy() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sequence did not finish")
}

func mustZone(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
