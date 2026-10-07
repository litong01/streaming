package avcontrol

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestProbeWalksTheListAndDoesNotSaveOrSwitchPower(t *testing.T) {
	path := writeFixture(t, fixtureWithKey)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &recordingCmd{states: map[string]Reading{"a": {On: true}}}
	c := New(path, Options{Commands: cmd})

	report, err := c.Probe(context.Background(), ConfigEdit{
		ProjectCode: "project-2",
		AccessID:    "access-1",
		Items: []ItemEdit{
			{ID: "a", Label: "Alpha", DeviceID: "dev-a", IP: "192.0.2.10", Protocol: "3.4", DelaySec: 2},
			{Label: "Gamma", DeviceID: "dev-c", Protocol: "3.4"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.Summary != "1 of 2 items are reachable" {
		t.Fatalf("report %+v", report)
	}
	if len(report.Items) != 2 || !report.Items[0].OK || report.Items[0].Summary != "Reachable" {
		t.Fatalf("first %#v", report.Items)
	}
	if report.Items[1].OK || report.Items[1].Summary != "Needs a local key" || report.Items[1].Label != "Gamma" {
		t.Fatalf("second %#v", report.Items[1])
	}
	if len(cmd.seen) != 1 || cmd.seen[0].ID != "a" || cmd.seen[0].LocalKey != "kept-key" {
		t.Fatalf("probed %#v", cmd.seen)
	}
	if cmd.sets != 0 {
		t.Fatal("probe turned a switch")
	}
	if cmd.file == nil || cmd.file.Developer.ProjectCode != "project-1" || cmd.file.Developer.AccessID != "access-1" {
		t.Fatal("probe replaced the saved credentials")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("probe wrote the file")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("probe left a temporary file")
	}
}

func TestProbeNamesThePathThatAnswered(t *testing.T) {
	path := writeFixture(t, fixtureWithKey)
	cmd := &pathCmd{reach: map[string]Reach{
		"a": {Via: "local"},
		"b": {Err: errNoReply},
	}}
	c := New(path, Options{Commands: cmd})
	report, err := c.Probe(context.Background(), ConfigEdit{
		AccessID: "access-1",
		Items: []ItemEdit{
			{ID: "a", Label: "Alpha", DeviceID: "dev-a", LocalKey: "typed-key-aaaa"},
			{ID: "b", Label: "Beta", DeviceID: "dev-b", IP: "192.0.2.20"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary != "1 of 2 items are reachable" {
		t.Fatalf("summary %q", report.Summary)
	}
	if report.Items[0].Summary != "Reachable on the local network" {
		t.Fatalf("local %#v", report.Items[0])
	}
	if report.Items[1].OK || !strings.Contains(report.Items[1].Summary, "Not reachable") {
		t.Fatalf("cloud %#v", report.Items[1])
	}
	if cmd.file == nil || cmd.file.Developer.AccessSecret != "secret-value" {
		t.Fatalf("secret %+v", cmd.file)
	}
	if len(cmd.switches) != 2 || cmd.switches[0].LocalKey != "typed-key-aaaa" || cmd.switches[1].LocalKey != "kept-key" {
		t.Fatalf("keys %#v", cmd.switches)
	}
}

func TestProbeDoesNotRetargetASavedKey(t *testing.T) {
	path := writeFixture(t, fixtureWithKey)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &recordingCmd{states: map[string]Reading{"b": {On: false}}}
	c := New(path, Options{Commands: cmd})
	report, err := c.Probe(context.Background(), ConfigEdit{
		ProjectCode: "project-1",
		AccessID:    "access-1",
		Items: []ItemEdit{
			{ID: "a", Label: "Alpha", DeviceID: "dev-a", IP: "192.0.2.99", Protocol: "3.4"},
			{ID: "b", Label: "Beta", DeviceID: "dev-b", IP: "192.0.2.20", Protocol: "3.4"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].OK || report.Items[0].Summary != "Enter the local key again for the new address" {
		t.Fatalf("moved %#v", report.Items[0])
	}
	if !report.Items[1].OK {
		t.Fatalf("unchanged %#v", report.Items[1])
	}
	for _, sw := range cmd.seen {
		if sw.ID == "a" || sw.LocalKey == "kept-key" && sw.IP == "192.0.2.99" {
			t.Fatalf("retargeted %#v", cmd.seen)
		}
	}
	if cmd.sets != 0 {
		t.Fatal("probe turned a switch")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("probe wrote the file")
	}

	secretCmd := &pathCmd{}
	c = New(path, Options{Commands: secretCmd})
	if _, err := c.Probe(context.Background(), ConfigEdit{
		AccessID: "access-2",
		Items: []ItemEdit{
			{ID: "a", Label: "Alpha", DeviceID: "dev-a", IP: "192.0.2.10", LocalKey: "0123456789abcdef"},
		},
	}); err == nil || !strings.Contains(err.Error(), "access secret again") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("access id: %v", err)
	}
	if secretCmd.file != nil {
		t.Fatal("probed with the saved secret")
	}
}

func TestProbeExplainsALocalKeyThatCannotBeUsed(t *testing.T) {
	client := newDeviceClient()
	file := &File{Control: Control{PreferLocal: true, CloudFallback: true}}
	got := client.Probe(context.Background(), file, []Switch{{
		ID: "a", Label: "Alpha", DeviceID: "dev-a", IP: "192.0.2.10", LocalKey: "testkey",
	}})
	if got["a"].Err == nil || got["a"].Err.Error() != "local: no usable local key; cloud: not configured" {
		t.Fatalf("%v", got["a"].Err)
	}
}

func TestProbeWithoutCloudDoesNotDial(t *testing.T) {
	client := newDeviceClient()
	file := &File{Control: Control{PreferLocal: true, CloudFallback: false}}
	got := client.Probe(context.Background(), file, []Switch{{
		ID: "a", Label: "Alpha", DeviceID: "dev-a", IP: "not-an-ip", LocalKey: "0123456789abcdef",
	}})
	if got["a"].Via != "" || got["a"].Err == nil || !strings.Contains(got["a"].Err.Error(), "local key") {
		t.Fatalf("%+v", got["a"])
	}
}

type recordingCmd struct {
	states map[string]Reading
	seen   []Switch
	file   *File
	sets   int
	used   int
}

func (r *recordingCmd) Set(context.Context, Switch, bool) error {
	r.sets++
	return nil
}

func (r *recordingCmd) States(_ context.Context, switches []Switch) map[string]Reading {
	r.seen = append([]Switch(nil), switches...)
	out := make(map[string]Reading, len(switches))
	for _, sw := range switches {
		if reading, ok := r.states[sw.ID]; ok {
			out[sw.ID] = reading
			continue
		}
		out[sw.ID] = Reading{Err: errNoReply}
	}
	return out
}

func (r *recordingCmd) Use(file *File) {
	r.used++
	r.file = file
}

type pathCmd struct {
	reach    map[string]Reach
	file     *File
	switches []Switch
}

func (p *pathCmd) Set(context.Context, Switch, bool) error { return nil }

func (p *pathCmd) States(context.Context, []Switch) map[string]Reading {
	return map[string]Reading{}
}

func (p *pathCmd) Use(*File) {}

func (p *pathCmd) Probe(_ context.Context, file *File, switches []Switch) map[string]Reach {
	p.file = file
	p.switches = append([]Switch(nil), switches...)
	return p.reach
}

const fixtureWithKey = `
server:
  timezone: "America/New_York"
control:
  prefer_local: true
  cloud_fallback: true
  command_mode: "absolute"
switches:
  - id: a
    label: Alpha
    device_id: dev-a
    ip: 192.0.2.10
    local_key: kept-key
  - id: b
    label: Beta
    device_id: dev-b
    ip: 192.0.2.20
    local_key: kept-key
startup:
  - { switch: a, settle_sec: 2 }
  - { switch: b, settle_sec: 5 }
shutdown: "reverse_of_startup"
schedule:
  ensure_off_daily: "17:00"
off_delay:
  step_minutes: 60
developer.tuya.com:
  project_code: project-1
  access_id: access-1
  access_secret: secret-value
`
