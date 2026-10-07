package avcontrol

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	phaseOff      = "off"
	phaseOn       = "on"
	phasePartial  = "partial"
	phaseStarting = "starting"
	phaseStopping = "stopping"
	phaseError    = "error"
	phaseUnknown  = "unknown"
	manualPause   = 5 * time.Minute
)

// Status is the row the AV page shows. It never includes keys or secrets.
type Status struct {
	Configured     bool     `json:"configured"`
	Phase          string   `json:"phase"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	Errors         []string `json:"errors,omitempty"`
	Busy           bool     `json:"busy"`
	Delayed        bool     `json:"delayed"`
	Held           bool     `json:"held"`
	OnLockedUntil  string   `json:"onLockedUntil,omitempty"`
	OffLockedUntil string   `json:"offLockedUntil,omitempty"`
}

// Options wires the AV controller into the streaming server. StreamActive is
// the Extron SMP; recording is reserved until a recorder signal exists.
type Options struct {
	StreamActive    func() bool
	RecordingActive func() bool
	Commands        Commander
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
}

// Controller runs the power sequences and the daily schedule on the streaming
// server's own process.
type Controller struct {
	path string

	mu            sync.Mutex
	file          *File
	mod           time.Time
	problem       string
	phase         string
	step          string
	errText       string
	readErr       map[string]string
	cmdErr        map[string]string
	fault         string
	busy          bool
	gen           int
	cancel        context.CancelFunc
	root          context.Context
	fired         map[string]bool
	pushed        *time.Time
	held          bool
	holdWhy       string
	holdTill      time.Time
	booted        time.Time
	lastManualOn  time.Time
	lastManualOff time.Time

	commands        Commander
	streamActive    func() bool
	recordingActive func() bool
	clock           func() time.Time
	sleeper         func(context.Context, time.Duration) error
}

func New(path string, opt Options) *Controller {
	commands := opt.Commands
	if commands == nil {
		commands = newDeviceClient()
	}
	now := time.Now()
	if opt.Now != nil {
		now = opt.Now()
	}
	sleeper := opt.Sleep
	if sleeper == nil {
		sleeper = sleepFor
	}
	return &Controller{
		path:            path,
		phase:           phaseUnknown,
		readErr:         map[string]string{},
		cmdErr:          map[string]string{},
		fired:           map[string]bool{},
		root:            context.Background(),
		booted:          now,
		commands:        commands,
		streamActive:    opt.StreamActive,
		recordingActive: opt.RecordingActive,
		clock:           opt.Now,
		sleeper:         sleeper,
	}
}

func (c *Controller) SetStreamActive(fn func() bool) {
	c.mu.Lock()
	c.streamActive = fn
	c.mu.Unlock()
}

// Run is the background loop. It sleeps between checks and only talks to the
// switches when a button or a scheduled time says to.
func (c *Controller) Run(ctx context.Context) {
	c.mu.Lock()
	c.root = ctx
	c.mu.Unlock()
	defer c.stopSequence()

	c.reload()
	if c.configured() {
		log.Printf("av control: config %s", c.path)
	} else {
		log.Printf("av control: %s", c.problemText())
	}
	c.sample(ctx)
	c.tick()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reload()
			if c.isBusy() {
				continue
			}
			c.sample(ctx)
			c.tick()
		}
	}
}

func (c *Controller) TurnOn() error {
	c.reload()
	if !c.configured() {
		return ErrNotConfigured
	}
	c.mu.Lock()
	now := c.nowLocked()
	if until, waiting := c.lockedUntil(now, c.lastManualOff); waiting {
		c.mu.Unlock()
		log.Printf("av control: on waits until %s", until.Format(time.RFC3339))
		return buttonLock{action: "On", until: until}
	}
	c.lastManualOn = now
	c.mu.Unlock()
	log.Printf("av control: on requested")
	c.begin(true)
	return nil
}

func (c *Controller) TurnOff() error {
	c.reload()
	if !c.configured() {
		return ErrNotConfigured
	}
	c.mu.Lock()
	now := c.nowLocked()
	if until, waiting := c.lockedUntil(now, c.lastManualOn); waiting {
		c.mu.Unlock()
		log.Printf("av control: off waits until %s", until.Format(time.RFC3339))
		return buttonLock{action: "Off", until: until}
	}
	c.mu.Unlock()
	if c.guardsManualOff() {
		if why := c.guardReason(); why != "" {
			c.noteHold(why)
			return nil
		}
	}
	c.mu.Lock()
	c.lastManualOff = c.nowLocked()
	c.mu.Unlock()
	log.Printf("av control: off requested")
	c.begin(false)
	return nil
}

func (c *Controller) DelayOff() error {
	c.reload()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return ErrNotConfigured
	}
	now := c.nowLocked()
	next, _ := c.nextOffLocked(now)
	if next.IsZero() {
		return ErrNoOff
	}
	step := c.file.OffDelay.StepMinutes
	if step <= 0 {
		step = 60
	}
	when := next.Add(time.Duration(step) * time.Minute)
	c.pushed = &when
	c.held = false
	c.holdWhy = ""
	c.holdTill = time.Time{}
	log.Printf("av control: shutdown delayed until %s", when.Format(time.RFC3339))
	return nil
}

func (c *Controller) Status() Status {
	c.reload()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *Controller) statusLocked() Status {
	now := c.nowLocked()
	if c.file == nil {
		return Status{
			Phase:  phaseOff,
			Status: "AV control is not configured",
			Error:  c.problem,
		}
	}
	next, delayed := c.nextOffLocked(now)
	text := composeStatus(c.phase, c.step, c.held, c.holdWhy, next, delayed, now)
	problems := c.problemsLocked()
	if len(problems) == 1 && !c.busy {
		text = "Needs attention · " + problems[0]
	}
	onUntil, offUntil := "", ""
	if until, waiting := c.lockedUntil(now, c.lastManualOff); waiting {
		onUntil = until.Format(time.RFC3339)
		if len(problems) == 0 && !c.busy {
			text += " · On waits until " + formatWhen(now, until)
		}
	}
	if until, waiting := c.lockedUntil(now, c.lastManualOn); waiting {
		offUntil = until.Format(time.RFC3339)
		if len(problems) == 0 && !c.busy {
			text += " · Off waits until " + formatWhen(now, until)
		}
	}
	return Status{
		Configured:     true,
		Phase:          c.phase,
		Status:         text,
		Error:          strings.Join(problems, "; "),
		Errors:         problems,
		Busy:           c.busy,
		Delayed:        delayed,
		Held:           c.held,
		OnLockedUntil:  onUntil,
		OffLockedUntil: offUntil,
	}
}

// lockedUntil is the thermostat-style pause. A manual On holds Off for five
// minutes, and a manual Off holds On for five minutes. The schedule does not
// use this pause.
func (c *Controller) lockedUntil(now, last time.Time) (time.Time, bool) {
	if last.IsZero() {
		return time.Time{}, false
	}
	until := last.Add(manualPause)
	if !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

type buttonLock struct {
	action string
	until  time.Time
}

func (e buttonLock) Error() string {
	return e.action + " waits until " + e.until.Format("3:04 PM")
}

func (e buttonLock) Unwrap() error {
	return ErrButtonLocked
}

// problemsLocked lists each outstanding read or command failure. The page
// shows them one at a time. A read error leaves when that switch answers. A
// command error leaves when a later command to that switch succeeds.
func (c *Controller) problemsLocked() []string {
	var out []string
	seen := map[string]bool{}
	if c.file != nil {
		for _, step := range c.file.Startup {
			if step.Switch == "" || seen[step.Switch] {
				continue
			}
			seen[step.Switch] = true
			if msg := c.readErr[step.Switch]; msg != "" {
				out = append(out, msg)
			}
			if msg := c.cmdErr[step.Switch]; msg != "" {
				out = append(out, msg)
			}
		}
	}
	if c.fault != "" {
		out = append(out, c.fault)
	}
	return out
}

func composeStatus(phase, step string, held bool, holdWhy string, next time.Time, delayed bool, now time.Time) string {
	if held && holdWhy != "" {
		return phaseWord(phase) + " · Shutdown held, " + holdWhy
	}
	if phase == phaseStarting || phase == phaseStopping {
		if step != "" {
			return phaseWord(phase) + " · " + step
		}
		return phaseWord(phase)
	}
	word := phaseWord(phase)
	if next.IsZero() {
		return word
	}
	when := "Off at " + formatWhen(now, next)
	if delayed {
		when += ", delayed"
	}
	if phase == phaseOff {
		when = "Next off " + formatWhen(now, next)
		if delayed {
			when += ", delayed"
		}
	}
	return word + " · " + when
}

func phaseWord(phase string) string {
	switch phase {
	case phaseOn:
		return "On"
	case phasePartial:
		return "Partly on"
	case phaseStarting:
		return "Turning on"
	case phaseStopping:
		return "Turning off"
	case phaseError:
		return "Needs attention"
	case phaseUnknown:
		return "Checking"
	default:
		return "Off"
	}
}

func formatWhen(now, when time.Time) string {
	when = when.In(now.Location())
	if now.Year() == when.Year() && now.YearDay() == when.YearDay() {
		return when.Format("3:04 PM")
	}
	return when.Format("Monday 3:04 PM")
}

func (c *Controller) tick() {
	c.mu.Lock()
	if c.file == nil || c.busy {
		c.mu.Unlock()
		return
	}
	snap := agenda{
		Now:    c.nowLocked(),
		Boot:   c.booted,
		File:   c.file,
		Fired:  c.fired,
		Pushed: c.pushed,
		Phase:  c.phase,
	}
	recheck := c.file.Guards.Recheck
	c.mu.Unlock()

	out := decide(snap)
	if out.StartOff {
		if why := c.guardReason(); why != "" {
			c.mu.Lock()
			if !c.nowLocked().Before(c.holdTill) {
				if recheck <= 0 {
					recheck = 5
				}
				c.holdTill = c.nowLocked().Add(time.Duration(recheck) * time.Minute)
				if !c.held {
					log.Printf("av control: shutdown held, %s", why)
				}
				c.held = true
				c.holdWhy = why
			}
			c.mu.Unlock()
			return
		}
	}

	c.mu.Lock()
	if out.ClearPush {
		c.pushed = nil
	}
	for _, key := range out.Mark {
		c.fired[key] = true
	}
	c.pruneFiredLocked(snap.Now)
	if out.StartOff || out.StartOn {
		c.held = false
		c.holdWhy = ""
	}
	c.mu.Unlock()

	switch {
	case out.StartOff:
		log.Printf("av control: scheduled off")
		c.begin(false)
	case out.StartOn:
		log.Printf("av control: scheduled on")
		c.begin(true)
	}
}

func (c *Controller) begin(powerOn bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	parent := c.root
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	c.gen++
	gen := c.gen
	c.cancel = cancel
	c.busy = true
	c.errText = ""
	c.held = false
	c.holdWhy = ""
	c.step = ""
	if powerOn {
		c.phase = phaseStarting
	} else {
		c.phase = phaseStopping
	}
	go c.run(ctx, gen, powerOn)
}

func (c *Controller) run(ctx context.Context, gen int, powerOn bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("av control sequence panicked: %v", recovered)
			c.finish(gen, fmt.Errorf("internal error"), false)
		}
	}()
	err := c.sequence(ctx, powerOn)
	if ctx.Err() != nil {
		c.mu.Lock()
		stale := gen != c.gen
		c.mu.Unlock()
		if stale {
			return
		}
	}
	confirmed := c.confirm(ctx, gen)
	c.finish(gen, err, confirmed)
}

func (c *Controller) finish(gen int, err error, confirmed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.busy = false
	c.step = ""
	c.cancel = nil
	if err != nil {
		log.Printf("av control: sequence failed: %s", shortErr(err))
		if !c.alreadyNoted(err) {
			c.fault = shortErr(err)
		}
	} else {
		c.fault = ""
	}
	if confirmed {
		return
	}
	if len(c.readErr) > 0 {
		return
	}
	c.phase = phaseError
	if c.fault == "" && err == nil {
		c.fault = "could not read the switches"
	}
}

func (c *Controller) noteCommand(sw Switch, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmdErr == nil {
		c.cmdErr = map[string]string{}
	}
	if reason == "" {
		delete(c.cmdErr, sw.ID)
		return
	}
	c.cmdErr[sw.ID] = switchLabel(sw) + ": command failed: " + reason
}

func (c *Controller) alreadyNoted(err error) bool {
	msg := shortErr(err)
	for _, noted := range c.cmdErr {
		if noted == msg {
			return true
		}
	}
	return false
}

func (c *Controller) sequence(ctx context.Context, powerOn bool) error {
	c.mu.Lock()
	file := c.file
	c.mu.Unlock()
	if file == nil {
		return ErrNotConfigured
	}
	steps := append([]Step(nil), file.Startup...)
	if !powerOn {
		steps = shutdownSteps(steps)
	}
	var runnable []Step
	for _, step := range steps {
		if step.Switch == "" {
			continue
		}
		runnable = append(runnable, step)
	}
	for i, step := range runnable {
		sw, ok := file.Switch(step.Switch)
		if !ok {
			return fmt.Errorf("unknown switch %s", step.Switch)
		}
		c.mu.Lock()
		c.step = sw.Label
		c.mu.Unlock()
		if err := c.commands.Set(ctx, sw, powerOn); err != nil {
			reason := shortErr(err)
			c.noteCommand(sw, reason)
			return fmt.Errorf("%s: command failed: %s", switchLabel(sw), reason)
		}
		c.noteCommand(sw, "")
		if err := ctx.Err(); err != nil {
			return err
		}
		if i == len(runnable)-1 || step.SettleSec <= 0 {
			continue
		}
		if err := c.sleeper(ctx, time.Duration(step.SettleSec)*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// confirm reads every startup switch and sets the phase from those reports.
// A command being accepted is not enough to call the system on or off.
func (c *Controller) confirm(ctx context.Context, gen int) bool {
	return c.readPhase(ctx, gen, true)
}

func (c *Controller) sample(ctx context.Context) {
	c.mu.Lock()
	busy := c.busy
	gen := c.gen
	c.mu.Unlock()
	if busy {
		return
	}
	c.readPhase(ctx, gen, false)
}

func (c *Controller) readPhase(ctx context.Context, gen int, required bool) bool {
	c.mu.Lock()
	file := c.file
	c.mu.Unlock()
	if file == nil {
		return false
	}
	seen := map[string]bool{}
	var switches []Switch
	for _, step := range file.Startup {
		if step.Switch == "" || seen[step.Switch] {
			continue
		}
		sw, ok := file.Switch(step.Switch)
		if !ok {
			continue
		}
		seen[step.Switch] = true
		switches = append(switches, sw)
	}
	if len(switches) == 0 {
		return false
	}
	states := c.commands.States(ctx, switches)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || (!required && c.busy) {
		return false
	}
	if c.readErr == nil {
		c.readErr = map[string]string{}
	}
	on := 0
	known := 0
	missed := 0
	for _, sw := range switches {
		reading, ok := states[sw.ID]
		if !ok || reading.Err != nil {
			reason := "no reply"
			if ok && reading.Err != nil {
				reason = shortErr(reading.Err)
			}
			c.readErr[sw.ID] = switchLabel(sw) + ": could not read status: " + reason
			missed++
			continue
		}
		delete(c.readErr, sw.ID)
		known++
		if reading.On {
			on++
		}
	}
	if missed > 0 {
		c.phase = phaseError
		return false
	}
	switch {
	case known == len(switches) && on == len(switches):
		c.phase = phaseOn
	case known == len(switches) && on == 0:
		c.phase = phaseOff
	default:
		c.phase = phasePartial
	}
	return true
}

func switchLabel(sw Switch) string {
	if sw.Label != "" {
		return sw.Label
	}
	return sw.ID
}

func (c *Controller) reload() {
	info, err := os.Stat(c.path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.file == nil {
			c.problem = fmt.Sprintf("config file not found (%s)", c.path)
		}
		return
	}
	if !info.ModTime().After(c.mod) && (c.file != nil || c.problem != "") {
		return
	}
	file, err := Load(c.path)
	c.mod = info.ModTime()
	if err != nil {
		c.problem = err.Error()
		log.Printf("av control: %s", c.problem)
		return
	}
	c.file = file
	c.problem = ""
	c.commands.Use(file)
}

func (c *Controller) guardReason() string {
	c.mu.Lock()
	file := c.file
	stream := c.streamActive
	recording := c.recordingActive
	c.mu.Unlock()
	if file == nil {
		return ""
	}
	var reasons []string
	for _, name := range file.Guards.Before {
		switch name {
		case "stream_active":
			if stream != nil && stream() {
				reasons = append(reasons, "a stream is active")
			}
		case "recording_active":
			if recording != nil && recording() {
				reasons = append(reasons, "a recording is active")
			}
		}
	}
	return strings.Join(reasons, " and ")
}

func (c *Controller) nextOffLocked(now time.Time) (time.Time, bool) {
	if c.file == nil {
		return time.Time{}, false
	}
	return nextOff(now.In(c.file.Location()), c.file, c.pushed)
}

func (c *Controller) nowLocked() time.Time {
	now := time.Now()
	if c.clock != nil {
		now = c.clock()
	}
	if c.file != nil {
		return now.In(c.file.Location())
	}
	return now
}

func (c *Controller) configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.file != nil
}

func (c *Controller) isBusy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy
}

func (c *Controller) problemText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.problem != "" {
		return c.problem
	}
	return "config file not found (" + c.path + ")"
}

func (c *Controller) pruneFiredLocked(now time.Time) {
	today := now.Format("2006-01-02")
	for key := range c.fired {
		day, _, _ := strings.Cut(key, "|")
		if day < today {
			delete(c.fired, key)
		}
	}
}

func (c *Controller) stopSequence() {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func sleepFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 180 {
		msg = msg[:180] + "…"
	}
	return msg
}

var (
	ErrNotConfigured = errors.New("AV control is not configured")
	ErrNoOff         = errors.New("there is no shutdown time to delay")
	ErrButtonLocked  = errors.New("button locked")
)

func (c *Controller) guardsManualOff() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return false
	}
	return !c.file.ManualOverridesGuards()
}

func (c *Controller) noteHold(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	recheck := 5
	if c.file != nil && c.file.Guards.Recheck > 0 {
		recheck = c.file.Guards.Recheck
	}
	if !c.nowLocked().Before(c.holdTill) {
		c.holdTill = c.nowLocked().Add(time.Duration(recheck) * time.Minute)
	}
	c.held = true
	c.holdWhy = why
}
