package avcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTunnelIsNotTheLanToScan(t *testing.T) {
	lan := net.Interface{Flags: net.FlagUp | net.FlagBroadcast | net.FlagRunning}
	tunnel := net.Interface{Flags: net.FlagUp | net.FlagPointToPoint | net.FlagRunning}
	down := net.Interface{Flags: net.FlagBroadcast}
	if !usableLAN(lan) || usableLAN(tunnel) || usableLAN(down) {
		t.Fatal("LAN filter kept a tunnel or a down interface")
	}
}

func TestDiscoveryPacketCarriesAddressAndProtocol(t *testing.T) {
	plain := []byte(`{"ip":"192.0.2.15","gwId":"dev-a","version":"3.4"}`)
	frame := packFrame(1, cmdDiscover, nil, plain, false)
	got, ok := decodeDiscoveryPacket(frame)
	if !ok || got.ID != "dev-a" || got.IP != "192.0.2.15" || got.Protocol != "3.4" {
		t.Fatalf("plain %#v %v", got, ok)
	}

	enc, err := encryptECB(udpKey(), plain)
	if err != nil {
		t.Fatal(err)
	}
	got, ok = decodeDiscoveryPacket(packFrame(1, cmdDiscover, nil, enc, false))
	if !ok || got.ID != "dev-a" || got.IP != "192.0.2.15" || got.Protocol != "3.4" {
		t.Fatalf("encrypted %#v %v", got, ok)
	}

	iv := bytes.Repeat([]byte{7}, 12)
	packet, err := pack6699(1, cmdDiscover, udpKey(), plain, iv)
	if err != nil {
		t.Fatal(err)
	}
	got, ok = decodeDiscoveryPacket(packet)
	if !ok || got.ID != "dev-a" || got.IP != "192.0.2.15" || got.Protocol != "3.4" {
		t.Fatalf("3.5 %#v %v", got, ok)
	}
}

func TestLanAddressIsPrivate(t *testing.T) {
	if lanIP("8.8.8.8") != "" || lanIP("100.69.0.1") != "" || lanIP("192.168.1.176") != "192.168.1.176" {
		t.Fatal("public and tunnel addresses were treated as a LAN address")
	}
}

func TestMergeUsesTheLanAddress(t *testing.T) {
	merged := mergeCatalog([]cloudDevice{{
		ID: "dev-1", Name: "Amp", LocalKey: "0123456789abcdef", Product: "Switch", IP: "8.8.8.8",
	}, {
		ID: "dev-2", Name: "Booth", LocalKey: "0123456789abcdef", IP: "192.168.1.20",
	}, {
		ID: "dev-3", Name: "Yard", LocalKey: "0123456789abcdef", IP: "192.168.1.30",
	}}, []lanDevice{
		{ID: "dev-1", IP: "192.168.1.15", Protocol: "3.5"},
		{ID: "dev-3", IP: "8.8.8.8", Protocol: "3.4"},
	})
	if len(merged) != 3 {
		t.Fatalf("merged %#v", merged)
	}
	if !merged[0].onNetwork || merged[0].ip != "192.168.1.15" || merged[0].protocol != "3.5" || merged[0].candidate != "" {
		t.Fatalf("lan %#v", merged[0])
	}
	if merged[1].onNetwork || merged[1].ip != "" || merged[1].candidate != "192.168.1.20" {
		t.Fatalf("candidate %#v", merged[1])
	}
	if merged[2].onNetwork || merged[2].ip != "" || merged[2].candidate != "192.168.1.30" {
		t.Fatalf("public announcement %#v", merged[2])
	}
}

func TestReplyMustBelongToTheDevice(t *testing.T) {
	if !replyMatchesDevice([]byte(`{"dps":{"1":true}}`), "dev-1") {
		t.Fatal("status reply was rejected")
	}
	if !replyMatchesDevice([]byte(`{"devId":"dev-1"}`), "dev-1") {
		t.Fatal("matching id was rejected")
	}
	if replyMatchesDevice([]byte(`{"devId":"other","dps":{"1":true}}`), "dev-1") {
		t.Fatal("another device was accepted")
	}
	if replyMatchesDevice([]byte(`{"error":"unvalid"}`), "dev-1") {
		t.Fatal("rejection was accepted")
	}
}

func TestDiscoveredKeyStaysWithItsAddress(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "av.yaml"), Options{})
	c.found = map[string]rememberedDevice{
		"dev-1": {key: "0123456789abcdef", ip: "192.168.1.20", protocol: "3.5"},
	}
	c.foundAt = time.Now()
	file := &File{Switches: []Switch{{ID: "a", DeviceID: "dev-a", LocalKey: "kept-key"}}}
	edit := c.withDiscoveredKeys(file, ConfigEdit{Items: []ItemEdit{
		{Label: "Moved", DeviceID: "dev-1", IP: "192.168.1.21"},
		{Label: "Amp", DeviceID: "dev-1", IP: "192.168.1.20"},
	}})
	if edit.Items[0].LocalKey != "" {
		t.Fatal("key followed a different address")
	}
	if edit.Items[1].LocalKey != "0123456789abcdef" || edit.Items[1].Protocol != "3.5" {
		t.Fatalf("matching address %+v", edit.Items[1])
	}
	c.foundAt = time.Now().Add(-discoverTTL - time.Second)
	stale := c.withDiscoveredKeys(file, ConfigEdit{Items: []ItemEdit{
		{Label: "Amp", DeviceID: "dev-1", IP: "192.168.1.20"},
	}})
	if stale.Items[0].LocalKey != "" {
		t.Fatal("expired scan still supplied a key")
	}
}

func TestScanRejectsASecondScan(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/token":
			_, _ = io.WriteString(w, `{"success":true,"result":{"access_token":"tok","expire_time":7200}}`)
		case "/v1.0/iot-01/associated-users/devices":
			once.Do(func() { close(started) })
			<-release
			_, _ = io.WriteString(w, `{"success":true,"result":{"has_more":false,"devices":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloud.Close()

	c := New(filepath.Join(t.TempDir(), "av.yaml"), Options{Seed: []byte(discoverSeed(cloud.URL))})
	edit := ConfigEdit{ProjectCode: "project-1", AccessID: "access-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Discover(ctx, edit)
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("scan did not start")
	}
	_, err := c.Discover(ctx, edit)
	close(release)
	if !errors.Is(err, errScanRunning) {
		t.Fatalf("second scan: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverKeepsLocalKeysOffThePage(t *testing.T) {
	const key = "0123456789abcdef"
	var detailCalls int
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.0/token":
			_, _ = io.WriteString(w, `{"success":true,"result":{"access_token":"tok","expire_time":7200}}`)
		case r.URL.Path == "/v1.0/iot-01/associated-users/devices":
			_, _ = io.WriteString(w, `{"success":true,"result":{"has_more":false,"devices":[{"id":"dev-1","name":"Amp","uid":"user-1","product_name":"Switch"}]}}`)
		case r.URL.Path == "/v1.0/devices/dev-1":
			detailCalls++
			fmt.Fprintf(w, `{"success":true,"result":{"id":"dev-1","name":"Amp","local_key":"%s"}}`, key)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloud.Close()

	path := filepath.Join(t.TempDir(), "avcontrol.yaml")
	c := New(path, Options{Seed: []byte(discoverSeed(cloud.URL))})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	report, err := c.Discover(ctx, ConfigEdit{ProjectCode: "project-1", AccessID: "access-1"})
	if err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 || len(report.Devices) != 1 || !report.Devices[0].HasLocalKey || report.Devices[0].Name != "Amp" {
		t.Fatalf("report %#v detail %d", report, detailCalls)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), key) {
		t.Fatalf("key left the server: %s", encoded)
	}

	view, err := c.SaveConfiguration(ConfigEdit{
		ProjectCode: "project-1",
		AccessID:    "access-1",
		Items: []ItemEdit{
			{ID: "a", Label: "Alpha", DeviceID: "dev-a", Protocol: "3.4", DelaySec: 2},
			{Label: "Amp", DeviceID: "dev-1", Protocol: "3.4", DelaySec: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Switches[0].LocalKey != "kept-key" {
		t.Fatalf("saved key replaced: %+v", saved.Switches[0])
	}
	if len(saved.Switches) != 2 || saved.Switches[1].LocalKey != key || saved.Switches[1].DeviceID != "dev-1" {
		t.Fatalf("imported %+v", saved.Switches)
	}
	page, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), key) || strings.Contains(string(page), "kept-key") {
		t.Fatalf("save view exposed a key: %s", page)
	}
}

func discoverSeed(endpoint string) string {
	return fmt.Sprintf(`
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
    local_key: kept-key
startup:
  - { switch: a, settle_sec: 2 }
shutdown: "reverse_of_startup"
off_delay:
  step_minutes: 60
developer.tuya.com:
  project_code: project-1
  access_id: access-1
  access_secret: secret-value
  endpoint: %q
`, endpoint)
}
