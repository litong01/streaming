package avcontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditorViewFollowsStartupOrder(t *testing.T) {
	file, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	view := file.editorView()
	if len(view.Items) != 2 || view.Items[0].ID != "a" || view.Items[1].ID != "b" {
		t.Fatalf("items %#v", view.Items)
	}
	if view.Items[0].DelaySec != 2 || view.Items[1].DelaySec != 5 {
		t.Fatalf("delays %#v", view.Items)
	}
	if !view.HasAccessSecret || view.AccessID != "test-id" {
		t.Fatalf("tuya %#v", view)
	}
}

func TestEditKeepsSecretsScheduleAndMacs(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "avcontrol", "avcontrol-seed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	view := file.editorView()
	if view.HasAccessSecret {
		t.Fatal("seed access secret counted as saved")
	}
	for _, item := range view.Items {
		if item.HasLocalKey {
			t.Fatalf("%s placeholder counted as a saved key", item.ID)
		}
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "REPLACE_LOCAL_KEY") || strings.Contains(string(encoded), "localKey") {
		t.Fatalf("secret left the device: %s", encoded)
	}

	edit := ConfigEdit{
		ProjectCode:  "project-2",
		AccessID:     "access-2",
		AccessSecret: "secret-2",
		Items:        make([]ItemEdit, len(view.Items)),
	}
	for i, item := range view.Items {
		edit.Items[i] = ItemEdit{
			ID:       item.ID,
			Label:    item.Label,
			DeviceID: item.DeviceID,
			IP:       item.IP,
			LocalKey: "key-" + item.ID,
			Protocol: item.Protocol,
			DelaySec: item.DelaySec,
		}
	}
	edit.Items[0].Label = "Video rack"
	edit.Items[0].DelaySec = 15
	// Last switch moves to the front. The wake step stays after booth-audio.
	last := edit.Items[len(edit.Items)-1]
	edit.Items = append([]ItemEdit{last}, edit.Items[:len(edit.Items)-1]...)

	out, next, err := applyEdit(source, file, edit)
	if err != nil {
		t.Fatal(err)
	}
	if next.Developer.AccessSecret != "secret-2" || next.Developer.ProjectCode != "project-2" {
		t.Fatalf("tuya %+v", next.Developer)
	}
	if next.Switches[0].ID != "amp-4" || next.Switches[0].LocalKey != "key-amp-4" {
		t.Fatalf("first switch %+v", next.Switches[0])
	}
	if next.Switches[1].Label != "Video rack" {
		t.Fatalf("renamed %#v", next.Switches[1])
	}
	sawWake := false
	for i, step := range next.Startup {
		if step.WakeMacs {
			sawWake = true
			if i == 0 || next.Startup[i-1].Switch != "booth-audio" {
				t.Fatalf("wake moved %#v", next.Startup)
			}
		}
	}
	if !sawWake {
		t.Fatal("wake step was dropped")
	}
	text := string(out)
	for _, keep := range []string{"Booth Mac 1", "ensure_off_daily", "America/New_York", "stream_active"} {
		if !strings.Contains(text, keep) {
			t.Fatalf("saved file lost %s\n%s", keep, text)
		}
	}
	if strings.Contains(text, "localKey") {
		t.Fatal(text)
	}
	reloaded, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Schedule.EnsureOffDaily != "17:00" || reloaded.Server.Timezone != "America/New_York" {
		t.Fatalf("reloaded schedule %+v zone %s", reloaded.Schedule, reloaded.Server.Timezone)
	}
}

func TestScheduleEditChangesOneDayAndKeepsTheOthers(t *testing.T) {
	file, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	view := file.editorView()
	if len(view.Days) != 7 || view.Days[0].ID != "sunday" || view.Days[0].On != "08:00" || view.Days[0].Off != "14:00" {
		t.Fatalf("sunday %#v", view.Days)
	}
	if view.Days[1].On != "" || view.Days[1].Off != "" || view.EnsureOff != "17:00" || view.DelayMinutes != 60 {
		t.Fatalf("rest %#v ensure %s delay %d", view.Days[1], view.EnsureOff, view.DelayMinutes)
	}

	days := make([]DayEdit, len(view.Days))
	for i, day := range view.Days {
		days[i] = DayEdit{ID: day.ID, On: day.On, Off: day.Off}
	}
	days[0].Off = "15:30"
	days[1] = DayEdit{ID: "monday", On: "09:00", Off: ""}
	ensure := ""
	minutes := 90
	out, next, err := applyEdit([]byte(fixtureYAML), file, ConfigEdit{
		ProjectCode:  "p",
		AccessID:     "test-id",
		AccessSecret: "test-secret",
		Items: []ItemEdit{{
			ID: "a", Label: "Alpha", DeviceID: "dev-a", LocalKey: "key-a", DelaySec: 1,
		}, {
			ID: "b", Label: "Beta", DeviceID: "dev-b", LocalKey: "key-b", DelaySec: 2,
		}},
		Days:         days,
		EnsureOff:    &ensure,
		DelayMinutes: &minutes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Schedule.Sunday.Off == nil || *next.Schedule.Sunday.Off != "15:30" {
		t.Fatalf("sunday off %#v", next.Schedule.Sunday.Off)
	}
	if next.Schedule.Monday.On == nil || *next.Schedule.Monday.On != "09:00" || next.Schedule.Monday.Off != nil {
		t.Fatalf("monday %#v", next.Schedule.Monday)
	}
	if next.Schedule.EnsureOffDaily != "" || next.OffDelay.StepMinutes != 90 {
		t.Fatalf("ensure %q delay %d", next.Schedule.EnsureOffDaily, next.OffDelay.StepMinutes)
	}
	reloaded, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Schedule.Monday.On == nil || *reloaded.Schedule.Monday.On != "09:00" || reloaded.OffDelay.StepMinutes != 90 {
		t.Fatalf("reloaded monday %#v delay %d", reloaded.Schedule.Monday, reloaded.OffDelay.StepMinutes)
	}
	if _, err := clockPtr("sunday on", "25:99"); err == nil {
		t.Fatal("accepted a bad clock")
	}
}

func TestEditReplacesALocalKeyAndRequiresOneForANewItem(t *testing.T) {
	file, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	file.Switches[0].LocalKey = "kept-secret"
	edit := ConfigEdit{
		ProjectCode:  "p",
		AccessID:     "test-id",
		AccessSecret: "test-secret",
		Items: []ItemEdit{{
			ID: "a", Label: "Alpha", DeviceID: "dev-a", DelaySec: 1,
		}, {
			Label: "Gamma", DeviceID: "dev-c", DelaySec: 4,
		}},
	}
	if _, _, err := applyEdit([]byte(fixtureYAML), file, edit); err == nil || !strings.Contains(err.Error(), "local key") {
		t.Fatalf("new item without a key: %v", err)
	}

	edit.Items[1].LocalKey = "gamma-key"
	_, next, err := applyEdit([]byte(fixtureYAML), file, edit)
	if err != nil {
		t.Fatal(err)
	}
	if next.Switches[0].LocalKey != "kept-secret" || next.Switches[1].ID != "gamma" || next.Switches[1].LocalKey != "gamma-key" {
		t.Fatalf("switches %+v", next.Switches)
	}
	if len(next.Startup) != 3 || next.Startup[0].Switch != "a" || !next.Startup[1].WakeMacs || next.Startup[2].SettleSec != 4 {
		t.Fatalf("startup %#v", next.Startup)
	}
}

func TestBlankKeyStaysOnlyForTheSameDeviceAndAddress(t *testing.T) {
	file, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	file.Switches[0].LocalKey = "kept-secret"
	file.Switches[1].LocalKey = "other-secret"
	file.Switches[0].IP = "192.0.2.10"
	same := editFor(file)
	_, next, err := applyEdit([]byte(fixtureYAML), file, same)
	if err != nil {
		t.Fatal(err)
	}
	if next.Switches[0].LocalKey != "kept-secret" {
		t.Fatalf("same address lost the key: %+v", next.Switches[0])
	}

	moved := editFor(file)
	moved.Items[0].IP = "192.0.2.99"
	if _, _, err := applyEdit([]byte(fixtureYAML), file, moved); err == nil || !strings.Contains(err.Error(), "local key again") || strings.Contains(err.Error(), "kept-secret") {
		t.Fatalf("changed address: %v", err)
	}
	other := editFor(file)
	other.Items[0].DeviceID = "dev-other"
	if _, _, err := applyEdit([]byte(fixtureYAML), file, other); err == nil || !strings.Contains(err.Error(), "local key again") || strings.Contains(err.Error(), "kept-secret") {
		t.Fatalf("changed device: %v", err)
	}

	file.Switches[0].IP = ""
	filled := editFor(file)
	filled.Items[0].IP = "192.0.2.10"
	_, next, err = applyEdit([]byte(fixtureYAML), file, filled)
	if err != nil {
		t.Fatal(err)
	}
	if next.Switches[0].LocalKey != "kept-secret" || next.Switches[0].IP != "192.0.2.10" {
		t.Fatalf("filling an empty address: %+v", next.Switches[0])
	}
}

func TestBlankAccessSecretStaysOnlyForTheSameAccessID(t *testing.T) {
	file, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	file.Switches[0].LocalKey = "kept-secret"
	file.Switches[1].LocalKey = "kept-secret"
	same := editFor(file)
	same.AccessSecret = ""
	_, next, err := applyEdit([]byte(fixtureYAML), file, same)
	if err != nil {
		t.Fatal(err)
	}
	if next.Developer.AccessSecret != "test-secret" || next.Developer.ProjectCode != "project-2" {
		t.Fatalf("tuya %+v", next.Developer)
	}

	renamed := editFor(file)
	renamed.AccessID = "access-2"
	renamed.AccessSecret = ""
	if _, _, err := applyEdit([]byte(fixtureYAML), file, renamed); err == nil || !strings.Contains(err.Error(), "access secret again") || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("changed access id: %v", err)
	}
}

func TestEditRejectsATargetThatCannotWork(t *testing.T) {
	file, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	file.Switches[0].LocalKey = "kept-secret"
	file.Switches[1].LocalKey = "kept-secret"
	badIP := editFor(file)
	badIP.Items[0].IP = "not-an-ip"
	if _, _, err := applyEdit([]byte(fixtureYAML), file, badIP); err == nil || !strings.Contains(err.Error(), "valid IP") {
		t.Fatalf("bad ip: %v", err)
	}
	badProtocol := editFor(file)
	badProtocol.Items[0].Protocol = "toggle"
	if _, _, err := applyEdit([]byte(fixtureYAML), file, badProtocol); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("bad protocol: %v", err)
	}
	sameDevice := editFor(file)
	sameDevice.Items[1].DeviceID = sameDevice.Items[0].DeviceID
	sameDevice.Items[1].LocalKey = "other-key"
	if _, _, err := applyEdit([]byte(fixtureYAML), file, sameDevice); err == nil || !strings.Contains(err.Error(), "same device id") {
		t.Fatalf("duplicate device: %v", err)
	}
}

func editFor(file *File) ConfigEdit {
	edit := ConfigEdit{
		ProjectCode:  "project-2",
		AccessID:     file.Developer.AccessID,
		AccessSecret: "typed-secret",
		Items:        make([]ItemEdit, len(file.Switches)),
	}
	for i, sw := range file.Switches {
		edit.Items[i] = ItemEdit{
			ID: sw.ID, Label: sw.Label, DeviceID: sw.DeviceID, IP: sw.IP, Protocol: "3.4", DelaySec: 1,
		}
	}
	return edit
}

func TestSaveConfigurationLeavesTheSeedBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	c := New(path, Options{Seed: []byte(fixtureYAML)})
	view, err := c.Configuration()
	if err != nil || !c.fromSeed || len(view.Items) != 2 {
		t.Fatalf("view %#v seed %t err %v", view, c.fromSeed, err)
	}
	_, err = c.SaveConfiguration(ConfigEdit{
		ProjectCode:  "p",
		AccessID:     "test-id",
		AccessSecret: "next-secret",
		Items: []ItemEdit{{
			ID: "b", Label: "Beta first", DeviceID: "dev-b", LocalKey: "beta-key", DelaySec: 3,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.fromSeed {
		t.Fatal("seed still in use")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	again := New(path, Options{})
	saved, err := again.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	if again.fromSeed || len(saved.Items) != 1 || saved.Items[0].Label != "Beta first" || saved.Items[0].DelaySec != 3 {
		t.Fatalf("saved %#v seed %t", saved, again.fromSeed)
	}
	if again.file.Developer.AccessSecret != "next-secret" {
		t.Fatalf("secret %q", again.file.Developer.AccessSecret)
	}
}
