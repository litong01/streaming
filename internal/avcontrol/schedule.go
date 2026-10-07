package avcontrol

import (
	"time"
)

// plan is what the scheduler should do at a single moment. A shutdown time
// that is already past when this process starts is marked and ignored. An On
// time is still caught up while the matching Off is in the future, so a
// server that starts during the service window turns the system on.
type plan struct {
	StartOn   bool
	StartOff  bool
	Mark      []string
	ClearPush bool
	NextOff   time.Time
	NextOn    time.Time
	Delayed   bool
}

type agenda struct {
	Now    time.Time
	Boot   time.Time
	File   *File
	Fired  map[string]bool
	Pushed *time.Time
	Phase  string
}

func decide(a agenda) plan {
	var out plan
	if a.File == nil {
		return out
	}
	now := a.Now.In(a.File.Location())
	boot := a.Boot.In(a.File.Location())
	out.NextOff, out.Delayed = nextOff(now, a.File, a.Pushed)
	out.NextOn = nextOn(now, a.File)

	if a.Pushed != nil && !now.Before(*a.Pushed) {
		out.ClearPush = true
		if !a.Pushed.Before(boot) && energized(a.Phase) {
			out.StartOff = true
		}
		out.Mark = append(out.Mark, fireKey(now, "off"), fireKey(now, "ensure"))
		return out
	}

	// A delayed Off replaces every earlier Off, including the daily safety net.
	if a.Pushed != nil && now.Before(*a.Pushed) {
		if on, ok := dueOn(now, a); ok {
			out.StartOn = on
		}
		out.Mark = append(out.Mark, dueOnMarks(now, a)...)
		return out
	}

	if on, ok := dueOn(now, a); ok {
		out.StartOn = on
	}
	out.Mark = append(out.Mark, dueOnMarks(now, a)...)

	if off, mark, ok := dueClock(now, boot, a, "off", todayOff(now, a.File)); ok {
		if off {
			out.StartOff = true
		}
		out.Mark = append(out.Mark, mark)
	}
	if !out.StartOff {
		if off, mark, ok := dueClock(now, boot, a, "ensure", todayEnsure(now, a.File)); ok {
			if off {
				out.StartOff = true
			}
			out.Mark = append(out.Mark, mark)
		}
	}
	return out
}

func dueOn(now time.Time, a agenda) (start bool, ok bool) {
	if fired(a.Fired, now, "on") {
		return false, false
	}
	onAt, offAt, inside := window(now, a.File)
	if !inside {
		if !onAt.IsZero() && !now.Before(onAt) && (offAt.IsZero() || !now.Before(offAt)) {
			return false, true
		}
		return false, false
	}
	if a.Phase == phaseOn {
		return false, true
	}
	return true, true
}

func dueOnMarks(now time.Time, a agenda) []string {
	start, ok := dueOn(now, a)
	if !ok {
		return nil
	}
	if start || a.Phase == phaseOn || !windowOpen(now, a.File) {
		return []string{fireKey(now, "on")}
	}
	return nil
}

func dueClock(now, boot time.Time, a agenda, kind string, at time.Time) (start bool, mark string, ok bool) {
	if at.IsZero() || fired(a.Fired, now, kind) || now.Before(at) {
		return false, "", false
	}
	mark = fireKey(now, kind)
	if at.Before(boot) || !energized(a.Phase) {
		return false, mark, true
	}
	return true, mark, true
}

func window(now time.Time, file *File) (onAt, offAt time.Time, inside bool) {
	day := file.Schedule.Day(now.Weekday())
	onAt, onOK := clockOn(now, day.On)
	offAt, _ = clockOn(now, day.Off)
	if !onOK {
		return onAt, offAt, false
	}
	if now.Before(onAt) {
		return onAt, offAt, false
	}
	if !offAt.IsZero() && !now.Before(offAt) {
		return onAt, offAt, false
	}
	return onAt, offAt, true
}

func windowOpen(now time.Time, file *File) bool {
	_, _, inside := window(now, file)
	return inside
}

func todayOff(now time.Time, file *File) time.Time {
	day := file.Schedule.Day(now.Weekday())
	at, _ := clockOn(now, day.Off)
	return at
}

func todayEnsure(now time.Time, file *File) time.Time {
	at, _ := clockOn(now, optionalClock(file.Schedule.EnsureOffDaily))
	return at
}

func nextOff(now time.Time, file *File, pushed *time.Time) (time.Time, bool) {
	if pushed != nil && pushed.After(now) {
		return pushed.In(file.Location()), true
	}
	var best time.Time
	consider := func(at time.Time) {
		if at.After(now) && (best.IsZero() || at.Before(best)) {
			best = at
		}
	}
	for i := 0; i < 8; i++ {
		day := now.In(file.Location()).AddDate(0, 0, i)
		spec := file.Schedule.Day(day.Weekday())
		if at, ok := clockOn(day, spec.Off); ok {
			consider(at)
		}
		if at, ok := clockOn(day, optionalClock(file.Schedule.EnsureOffDaily)); ok {
			consider(at)
		}
	}
	return best, false
}

func nextOn(now time.Time, file *File) time.Time {
	var best time.Time
	for i := 0; i < 8; i++ {
		day := now.In(file.Location()).AddDate(0, 0, i)
		spec := file.Schedule.Day(day.Weekday())
		at, ok := clockOn(day, spec.On)
		if !ok || !at.After(now) {
			continue
		}
		if best.IsZero() || at.Before(best) {
			best = at
		}
	}
	return best
}

func energized(phase string) bool {
	switch phase {
	case phaseOn, phasePartial, phaseError, phaseStarting:
		return true
	default:
		return false
	}
}

func fired(fired map[string]bool, now time.Time, kind string) bool {
	return fired[fireKey(now, kind)]
}

func fireKey(now time.Time, kind string) string {
	return now.Format("2006-01-02") + "|" + kind
}

func shutdownSteps(startup []Step) []Step {
	var steps []Step
	for i := len(startup) - 1; i >= 0; i-- {
		step := startup[i]
		if step.Switch == "" {
			continue
		}
		steps = append(steps, step)
	}
	return steps
}
