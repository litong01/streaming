package avcontrol

import (
	"fmt"
	"os"
	"strings"
	"time"

	// Embed the zone database so America/New_York works on the Android
	// tablet, which does not ship a system zoneinfo directory.
	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

// File is the AV power configuration. The tablet keeps the real file beside
// the streaming config; secrets in it never go to the browser.
type File struct {
	Server    ServerSection `yaml:"server"`
	Control   Control       `yaml:"control"`
	Switches  []Switch      `yaml:"switches"`
	Startup   []Step        `yaml:"startup"`
	Shutdown  string        `yaml:"shutdown"`
	Schedule  Schedule      `yaml:"schedule"`
	OffDelay  OffDelay      `yaml:"off_delay"`
	Guards    Guards        `yaml:"guards"`
	Developer Developer     `yaml:"developer.tuya.com"`

	location *time.Location
}

type ServerSection struct {
	Timezone string `yaml:"timezone"`
	LogLevel string `yaml:"log_level"`
}

type Control struct {
	PreferLocal   bool   `yaml:"prefer_local"`
	CloudFallback bool   `yaml:"cloud_fallback"`
	CommandMode   string `yaml:"command_mode"`
}

type Switch struct {
	ID           string `yaml:"id"`
	Label        string `yaml:"label"`
	DeviceID     string `yaml:"device_id"`
	IP           string `yaml:"ip"`
	LocalKey     string `yaml:"local_key"`
	Protocol     string `yaml:"protocol"`
	Circuit      string `yaml:"circuit"`
	PowerRestore string `yaml:"power_restore"`
}

type Step struct {
	Switch    string `yaml:"switch"`
	WakeMacs  bool   `yaml:"wake_macs"`
	SettleSec int    `yaml:"settle_sec"`
}

type Schedule struct {
	Sunday         Day    `yaml:"sunday"`
	Monday         Day    `yaml:"monday"`
	Tuesday        Day    `yaml:"tuesday"`
	Wednesday      Day    `yaml:"wednesday"`
	Thursday       Day    `yaml:"thursday"`
	Friday         Day    `yaml:"friday"`
	Saturday       Day    `yaml:"saturday"`
	EnsureOffDaily string `yaml:"ensure_off_daily"`
}

type Day struct {
	On  *string `yaml:"on"`
	Off *string `yaml:"off"`
}

type OffDelay struct {
	StepMinutes int `yaml:"step_minutes"`
}

type Guards struct {
	Before        []string `yaml:"before_scheduled_off"`
	OnActive      string   `yaml:"on_guard_active"`
	Recheck       int      `yaml:"recheck_minutes"`
	StopOverrides *bool    `yaml:"stop_now_overrides_guards"`
}

type Developer struct {
	ProjectCode  string `yaml:"project_code"`
	AccessID     string `yaml:"access_id"`
	AccessSecret string `yaml:"access_secret"`
	Endpoint     string `yaml:"endpoint"`
}

func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read av control config: %w", err)
	}
	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse av control config: %w", err)
	}
	if err := file.validate(); err != nil {
		return nil, err
	}
	return &file, nil
}

func (f *File) validate() error {
	zone := strings.TrimSpace(f.Server.Timezone)
	if zone == "" {
		return fmt.Errorf("av control config: server.timezone is required")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return fmt.Errorf("av control config: timezone %q: %w", zone, err)
	}
	f.location = loc
	f.Server.Timezone = zone

	mode := strings.ToLower(strings.TrimSpace(f.Control.CommandMode))
	if mode == "" {
		mode = "absolute"
	}
	if mode != "absolute" {
		return fmt.Errorf("av control config: command_mode must be absolute")
	}
	f.Control.CommandMode = mode

	if shutdown := strings.TrimSpace(f.Shutdown); shutdown != "" && shutdown != "reverse_of_startup" {
		return fmt.Errorf("av control config: shutdown %q is not supported", shutdown)
	}
	f.Shutdown = "reverse_of_startup"

	if len(f.Switches) == 0 {
		return fmt.Errorf("av control config: no switches")
	}
	byID := make(map[string]Switch, len(f.Switches))
	for i := range f.Switches {
		sw := &f.Switches[i]
		sw.ID = strings.TrimSpace(sw.ID)
		sw.Label = strings.TrimSpace(sw.Label)
		sw.DeviceID = strings.TrimSpace(sw.DeviceID)
		sw.IP = strings.TrimSpace(sw.IP)
		sw.LocalKey = strings.TrimSpace(sw.LocalKey)
		sw.Protocol = strings.TrimSpace(sw.Protocol)
		if sw.ID == "" {
			return fmt.Errorf("av control config: a switch is missing an id")
		}
		if sw.DeviceID == "" {
			return fmt.Errorf("av control config: switch %s is missing a device_id", sw.ID)
		}
		if sw.Label == "" {
			sw.Label = sw.ID
		}
		if _, ok := byID[sw.ID]; ok {
			return fmt.Errorf("av control config: duplicate switch %s", sw.ID)
		}
		byID[sw.ID] = *sw
	}

	if len(f.Startup) == 0 {
		return fmt.Errorf("av control config: startup sequence is empty")
	}
	sawSwitch := false
	for _, step := range f.Startup {
		if step.WakeMacs && step.Switch == "" {
			continue
		}
		if step.Switch == "" {
			return fmt.Errorf("av control config: a startup step has no switch")
		}
		if _, ok := byID[step.Switch]; !ok {
			return fmt.Errorf("av control config: startup step %s is not a switch", step.Switch)
		}
		if step.SettleSec < 0 {
			return fmt.Errorf("av control config: switch %s has a negative settle time", step.Switch)
		}
		sawSwitch = true
	}
	if !sawSwitch {
		return fmt.Errorf("av control config: startup sequence has no switches")
	}

	for _, day := range []struct {
		name string
		day  Day
	}{
		{"sunday", f.Schedule.Sunday},
		{"monday", f.Schedule.Monday},
		{"tuesday", f.Schedule.Tuesday},
		{"wednesday", f.Schedule.Wednesday},
		{"thursday", f.Schedule.Thursday},
		{"friday", f.Schedule.Friday},
		{"saturday", f.Schedule.Saturday},
	} {
		if err := checkClock(day.name+" on", day.day.On); err != nil {
			return err
		}
		if err := checkClock(day.name+" off", day.day.Off); err != nil {
			return err
		}
	}
	if err := checkClock("ensure_off_daily", optionalClock(f.Schedule.EnsureOffDaily)); err != nil {
		return err
	}

	if f.OffDelay.StepMinutes <= 0 {
		f.OffDelay.StepMinutes = 60
	}
	if f.Guards.Recheck <= 0 {
		f.Guards.Recheck = 5
	}
	if f.needsCloud() {
		f.Developer.AccessID = strings.TrimSpace(f.Developer.AccessID)
		f.Developer.AccessSecret = strings.TrimSpace(f.Developer.AccessSecret)
		f.Developer.Endpoint = strings.TrimRight(strings.TrimSpace(f.Developer.Endpoint), "/")
		if f.Developer.AccessID == "" || f.Developer.AccessSecret == "" {
			return fmt.Errorf("av control config: Tuya access id and secret are required")
		}
		if f.Developer.Endpoint == "" {
			f.Developer.Endpoint = defaultCloudEndpoint
		}
	}
	return nil
}

func (f *File) needsCloud() bool {
	if !f.Control.PreferLocal {
		return true
	}
	return f.Control.CloudFallback
}

func (f *File) Location() *time.Location {
	if f.location != nil {
		return f.location
	}
	return time.Local
}

func (f *File) Switch(id string) (Switch, bool) {
	for _, sw := range f.Switches {
		if sw.ID == id {
			return sw, true
		}
	}
	return Switch{}, false
}

func (f *File) ManualOverridesGuards() bool {
	if f.Guards.StopOverrides == nil {
		return true
	}
	return *f.Guards.StopOverrides
}

func (s Schedule) Day(weekday time.Weekday) Day {
	switch weekday {
	case time.Sunday:
		return s.Sunday
	case time.Monday:
		return s.Monday
	case time.Tuesday:
		return s.Tuesday
	case time.Wednesday:
		return s.Wednesday
	case time.Thursday:
		return s.Thursday
	case time.Friday:
		return s.Friday
	default:
		return s.Saturday
	}
}

func optionalClock(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func checkClock(name string, value *string) error {
	if value == nil {
		return nil
	}
	text := strings.TrimSpace(*value)
	if text == "" || strings.EqualFold(text, "null") {
		return nil
	}
	if _, err := time.Parse("15:04", text); err != nil {
		return fmt.Errorf("av control config: %s time %q is not HH:MM", name, text)
	}
	return nil
}

func clockOn(day time.Time, value *string) (time.Time, bool) {
	if value == nil {
		return time.Time{}, false
	}
	text := strings.TrimSpace(*value)
	if text == "" || strings.EqualFold(text, "null") {
		return time.Time{}, false
	}
	parsed, err := time.Parse("15:04", text)
	if err != nil {
		return time.Time{}, false
	}
	y, m, d := day.Date()
	return time.Date(y, m, d, parsed.Hour(), parsed.Minute(), 0, 0, day.Location()), true
}
