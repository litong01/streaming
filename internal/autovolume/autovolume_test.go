package autovolume

import (
	"testing"
	"time"
)

var start = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

const poll = 500 * time.Millisecond

// room plays a source into a controller as the SMP would meter it: the level
// heard is the source plus the gain, and every change asked for is applied.
type room struct {
	t       *testing.T
	c       *Controller
	now     time.Time
	gain    int
	changes []time.Time
}

func newRoom(t *testing.T, gain int) *room {
	return &room{t: t, c: New(DefaultSettings()), now: start, gain: gain}
}

func (r *room) play(source int, d time.Duration) Decision {
	var last Decision
	for end := r.now.Add(d); r.now.Before(end); r.now = r.now.Add(poll) {
		level := source + r.gain
		if source <= -900 {
			level = -900
		}
		last = r.c.Observe(Reading{
			At: r.now, LeftLevelTenths: level, RightLevelTenths: level, GainTenths: r.gain,
		})
		if last.Change {
			r.c.Applied(r.now, r.gain, last.GainTenths)
			r.gain = last.GainTenths
			r.changes = append(r.changes, r.now)
		}
	}
	return last
}

func (r *room) checkGainInSafeRange() {
	r.t.Helper()
	s := DefaultSettings()
	if r.gain < s.MinGainTenths || r.gain > s.MaxGainTenths {
		r.t.Fatalf("gain %d left the safe range", r.gain)
	}
}

func TestQuietSourceIsBroughtUpToTarget(t *testing.T) {
	r := newRoom(t, 0)
	r.play(-300, 2*time.Minute)
	level := -300 + r.gain
	s := DefaultSettings()
	if level < s.TargetTenths-s.DeadbandTenths || level > s.TargetTenths+s.DeadbandTenths {
		t.Fatalf("level settled at %d with gain %d", level, r.gain)
	}
}

func TestLoudSourceIsBroughtDownToTarget(t *testing.T) {
	r := newRoom(t, 0)
	r.play(-80, time.Minute)
	level := -80 + r.gain
	s := DefaultSettings()
	if level < s.TargetTenths-s.DeadbandTenths || level > s.TargetTenths+s.DeadbandTenths {
		t.Fatalf("level settled at %d with gain %d", level, r.gain)
	}
}

func TestSilenceIsNeverTurnedUp(t *testing.T) {
	r := newRoom(t, 0)
	if d := r.play(-900, 5*time.Minute); d.Note != NoteWaiting {
		t.Fatalf("note %q", d.Note)
	}
	if len(r.changes) != 0 || r.gain != 0 {
		t.Fatalf("silence moved the gain to %d", r.gain)
	}
}

func TestBackgroundHissIsNeverTurnedUp(t *testing.T) {
	r := newRoom(t, 0)
	r.play(-550, 5*time.Minute)
	if r.gain != 0 {
		t.Fatalf("hiss moved the gain to %d", r.gain)
	}
}

func TestGainNeverPassesTheSafeLimits(t *testing.T) {
	r := newRoom(t, 0)
	r.play(-480, 5*time.Minute)
	if d := r.c.Observe(Reading{At: r.now, LeftLevelTenths: -480 + r.gain, RightLevelTenths: -480 + r.gain, GainTenths: r.gain}); d.Change {
		t.Fatalf("asked to change past the limit: %+v", d)
	}
	if r.gain != DefaultSettings().MaxGainTenths {
		t.Fatalf("very quiet source left gain at %d", r.gain)
	}

	r = newRoom(t, 0)
	r.play(100, 5*time.Minute)
	if r.gain != DefaultSettings().MinGainTenths {
		t.Fatalf("very loud source left gain at %d", r.gain)
	}
}

func TestClippingCutsAtOnce(t *testing.T) {
	r := newRoom(t, 0)
	r.play(-180, 10*time.Second)
	before := r.gain
	r.play(0, poll)
	if r.gain != before-DefaultSettings().ClipStepTenths {
		t.Fatalf("clip moved gain from %d to %d", before, r.gain)
	}
}

func TestNoTwoChangesWithinTheMinimumInterval(t *testing.T) {
	r := newRoom(t, 0)
	for _, source := range []int{-80, 0, -400, -100, 50, -350} {
		r.play(source, 20*time.Second)
		r.checkGainInSafeRange()
	}
	for i := 1; i < len(r.changes); i++ {
		if gap := r.changes[i].Sub(r.changes[i-1]); gap < DefaultSettings().MinInterval {
			t.Fatalf("changes %v apart", gap)
		}
	}
}

func TestRaiseWaitsAfterACut(t *testing.T) {
	r := newRoom(t, 0)
	r.play(-180, 10*time.Second)
	r.play(0, poll)
	cut := r.now
	r.play(-400, DefaultSettings().RaiseHoldAfterCut-poll)
	if len(r.changes) != 1 {
		t.Fatalf("raised %v after a cut", r.changes[len(r.changes)-1].Sub(cut))
	}
}

func TestSmallWanderingInsideTheDeadbandChangesNothing(t *testing.T) {
	r := newRoom(t, 0)
	for i := 0; i < 60; i++ {
		r.play(-150-(i%2)*60, 2*time.Second)
	}
	if len(r.changes) != 0 {
		t.Fatalf("made %d changes", len(r.changes))
	}
}

func TestMutedHoldsAndForgetsReadings(t *testing.T) {
	c := New(DefaultSettings())
	for i := 0; i < 10; i++ {
		c.Observe(Reading{At: start.Add(time.Duration(i) * poll), LeftLevelTenths: -400, RightLevelTenths: -400})
	}
	d := c.Observe(Reading{At: start.Add(10 * poll), LeftLevelTenths: 0, RightLevelTenths: 0, Muted: true})
	if d.Change || d.Note != NoteMuted {
		t.Fatalf("muted gave %+v", d)
	}
	d = c.Observe(Reading{At: start.Add(11 * poll), LeftLevelTenths: -400, RightLevelTenths: -400})
	if d.Change {
		t.Fatalf("acted on readings from before the mute: %+v", d)
	}
}

func TestLouderChannelDecides(t *testing.T) {
	c := New(DefaultSettings())
	var d Decision
	for i := 0; i < 10; i++ {
		d = c.Observe(Reading{At: start.Add(time.Duration(i) * poll), LeftLevelTenths: -400, RightLevelTenths: -60})
	}
	if !d.Change || d.GainTenths >= 0 {
		t.Fatalf("quiet left channel outweighed loud right: %+v", d)
	}
}

func TestGainSetByHandOutsideTheRangeIsWalkedBack(t *testing.T) {
	r := newRoom(t, 240)
	r.play(-900, 30*time.Second)
	if r.gain != DefaultSettings().MaxGainTenths {
		t.Fatalf("gain left at %d", r.gain)
	}
}

func TestStaleReadingsAreForgotten(t *testing.T) {
	c := New(DefaultSettings())
	for i := 0; i < 10; i++ {
		c.Observe(Reading{At: start.Add(time.Duration(i) * poll), LeftLevelTenths: -400, RightLevelTenths: -400})
	}
	d := c.Observe(Reading{At: start.Add(time.Minute), LeftLevelTenths: -400, RightLevelTenths: -400})
	if d.Change {
		t.Fatalf("acted on readings a minute old: %+v", d)
	}
}
