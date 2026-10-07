package avcontrol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAcceptsTheScheduleSchema(t *testing.T) {
	path := writeFixture(t, fixtureYAML)
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Location().String() != "America/New_York" {
		t.Fatalf("zone %s", file.Location())
	}
	if got := shutdownSteps(file.Startup); len(got) != 2 || got[0].Switch != "b" || got[1].Switch != "a" {
		t.Fatalf("shutdown steps %#v", got)
	}
	on := "08:00"
	if file.Schedule.Sunday.On == nil || *file.Schedule.Sunday.On != on {
		t.Fatalf("sunday on %#v", file.Schedule.Sunday.On)
	}
	if file.Schedule.Monday.On != nil {
		t.Fatal("monday on should be empty")
	}
	if file.OffDelay.StepMinutes != 60 || !file.ManualOverridesGuards() {
		t.Fatalf("delay %d overrides %t", file.OffDelay.StepMinutes, file.ManualOverridesGuards())
	}
}

func TestLoadRejectsToggle(t *testing.T) {
	yaml := strings.Replace(fixtureYAML, `command_mode: "absolute"`, `command_mode: "toggle"`, 1)
	_, err := Load(writeFixture(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error %v", err)
	}
}

func TestLoadRejectsAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	if err := os.WriteFile(path, make([]byte, 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error %v", err)
	}
}

func TestExampleFileParsesWhenPresent(t *testing.T) {
	path := filepath.Join("..", "..", "avcontrol", "avcontrol.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("example config is not on this machine")
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func writeFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const fixtureYAML = `
server:
  timezone: "America/New_York"
control:
  prefer_local: false
  cloud_fallback: true
  command_mode: "absolute"
switches:
  - id: a
    label: Alpha
    device_id: dev-a
  - id: b
    label: Beta
    device_id: dev-b
startup:
  - { switch: a, settle_sec: 2 }
  - { wake_macs: true, settle_sec: 0 }
  - { switch: b, settle_sec: 5 }
shutdown: "reverse_of_startup"
schedule:
  sunday: { on: "08:00", off: "14:00" }
  monday: { on: null, off: null }
  ensure_off_daily: "17:00"
off_delay:
  step_minutes: 60
guards:
  before_scheduled_off: ["stream_active", "recording_active"]
  recheck_minutes: 5
  stop_now_overrides_guards: true
developer.tuya.com:
  access_id: test-id
  access_secret: test-secret
`
