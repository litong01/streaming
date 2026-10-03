// Package autovolume decides how the stream's gain should move so the level
// going out stays steady while the level coming in does not. It only decides;
// the server reads the SMP's meters, asks for a decision, and writes the gain.
//
// The SMP's meter is read as the level after its gain, the level that goes
// out: a change of gain is expected to show up on the next readings. That is
// why the readings gathered before a change are dropped once it is applied.
package autovolume

import "time"

// Settings are in tenths of a decibel, the unit the SMP itself uses.
type Settings struct {
	// TargetTenths is the average meter reading aimed for, and DeadbandTenths
	// how far either side of it the average may wander before anything moves.
	TargetTenths   int
	DeadbandTenths int
	// Readings at or below SilenceTenths are ignored, so a pause or the hiss
	// of an idle source is never turned up.
	SilenceTenths int
	// A reading at or above ClipTenths lowers the gain at once, by
	// ClipStepTenths, without waiting for an average.
	ClipTenths     int
	ClipStepTenths int
	// The automatic gain never leaves this range, which is narrower than the
	// -18 to +24 dB the SMP accepts.
	MinGainTenths int
	MaxGainTenths int
	// The most one change may raise or lower the gain.
	RaiseStepTenths int
	LowerStepTenths int
	// Window is how far back readings are averaged, and MinReadings how many
	// non-silent ones it must hold before the average is trusted.
	Window      time.Duration
	MinReadings int
	// The least time between two changes, between two raises, and between a
	// cut and the next raise. Raising slowly and cutting quickly is what keeps
	// the volume from pumping.
	MinInterval       time.Duration
	RaiseInterval     time.Duration
	RaiseHoldAfterCut time.Duration
}

func DefaultSettings() Settings {
	return Settings{
		TargetTenths:      -180,
		DeadbandTenths:    40,
		SilenceTenths:     -500,
		ClipTenths:        -30,
		ClipStepTenths:    20,
		MinGainTenths:     -120,
		MaxGainTenths:     180,
		RaiseStepTenths:   10,
		LowerStepTenths:   30,
		Window:            4 * time.Second,
		MinReadings:       6,
		MinInterval:       time.Second,
		RaiseInterval:     2 * time.Second,
		RaiseHoldAfterCut: 5 * time.Second,
	}
}

// Reading is one poll of the SMP.
type Reading struct {
	At               time.Time
	LeftLevelTenths  int
	RightLevelTenths int
	GainTenths       int
	Muted            bool
}

// Decision says whether to change the gain, to what, and in a word or two
// what the controller is doing, for the control page.
type Decision struct {
	Change     bool
	GainTenths int
	Note       string
}

const (
	NoteOff       = "Off"
	NoteWaiting   = "Waiting for sound"
	NoteSteady    = "Steady"
	NoteRaising   = "Raising"
	NoteLowering  = "Lowering"
	NoteTooLoud   = "Too loud, lowering"
	NoteMuted     = "Muted"
	NoteAtHighest = "At highest auto volume"
	NoteAtLowest  = "At lowest auto volume"
)

type sample struct {
	at    time.Time
	level int
}

type Controller struct {
	settings   Settings
	samples    []sample
	lastChange time.Time
	lastRaise  time.Time
	lastCut    time.Time
}

func New(settings Settings) *Controller {
	return &Controller{settings: settings}
}

// Reset forgets every reading and every change, as when the automatic
// control is switched on.
func (c *Controller) Reset() {
	c.samples = c.samples[:0]
	c.lastChange = time.Time{}
	c.lastRaise = time.Time{}
	c.lastCut = time.Time{}
}

// Applied records that the gain was changed from one value to another.
func (c *Controller) Applied(at time.Time, from, to int) {
	c.lastChange = at
	if to > from {
		c.lastRaise = at
	} else {
		c.lastCut = at
	}
	c.samples = c.samples[:0]
}

func (c *Controller) Observe(r Reading) Decision {
	s := c.settings
	gain := r.GainTenths
	if r.Muted {
		c.samples = c.samples[:0]
		return hold(NoteMuted)
	}
	c.dropBefore(r.At.Add(-s.Window))

	level := max(r.LeftLevelTenths, r.RightLevelTenths)
	canChange := c.since(c.lastChange, r.At) >= s.MinInterval

	if level >= s.ClipTenths {
		if gain <= s.MinGainTenths {
			return hold(NoteAtLowest)
		}
		if canChange {
			return change(max(gain-s.ClipStepTenths, s.MinGainTenths), NoteTooLoud)
		}
	}
	if level > s.SilenceTenths {
		c.samples = append(c.samples, sample{at: r.At, level: level})
	}

	// A gain set by hand outside the automatic range is walked back into it
	// at the usual pace, whatever the room sounds like.
	if gain > s.MaxGainTenths {
		if !canChange {
			return hold(NoteLowering)
		}
		return change(max(gain-s.LowerStepTenths, s.MaxGainTenths), NoteLowering)
	}
	if gain < s.MinGainTenths {
		if !canChange || c.since(c.lastRaise, r.At) < s.RaiseInterval {
			return hold(NoteRaising)
		}
		return change(min(gain+s.RaiseStepTenths, s.MinGainTenths), NoteRaising)
	}

	if len(c.samples) < s.MinReadings {
		return hold(NoteWaiting)
	}
	offset := s.TargetTenths - c.average()
	switch {
	case offset < -s.DeadbandTenths:
		if gain <= s.MinGainTenths {
			return hold(NoteAtLowest)
		}
		if !canChange {
			return hold(NoteLowering)
		}
		step := min(wholeDecibels(-offset), s.LowerStepTenths)
		return change(max(gain-step, s.MinGainTenths), NoteLowering)
	case offset > s.DeadbandTenths:
		if gain >= s.MaxGainTenths {
			return hold(NoteAtHighest)
		}
		if !canChange ||
			c.since(c.lastRaise, r.At) < s.RaiseInterval ||
			c.since(c.lastCut, r.At) < s.RaiseHoldAfterCut {
			return hold(NoteRaising)
		}
		step := min(wholeDecibels(offset), s.RaiseStepTenths)
		return change(min(gain+step, s.MaxGainTenths), NoteRaising)
	}
	return hold(NoteSteady)
}

func (c *Controller) dropBefore(cutoff time.Time) {
	kept := c.samples[:0]
	for _, sample := range c.samples {
		if sample.at.After(cutoff) {
			kept = append(kept, sample)
		}
	}
	c.samples = kept
}

func (c *Controller) average() int {
	total := 0
	for _, sample := range c.samples {
		total += sample.level
	}
	return total / len(c.samples)
}

// since treats "never" as long ago, and a clock that stepped backwards as
// just now, so neither can unlock a change early.
func (c *Controller) since(then, now time.Time) time.Duration {
	if then.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	if now.Before(then) {
		return 0
	}
	return now.Sub(then)
}

func wholeDecibels(tenths int) int {
	return tenths / 10 * 10
}

func hold(note string) Decision {
	return Decision{Note: note}
}

func change(gain int, note string) Decision {
	return Decision{Change: true, GainTenths: gain, Note: note}
}
