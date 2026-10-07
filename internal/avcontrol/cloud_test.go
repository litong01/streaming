package avcontrol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCloudSignsCommandsAndCachesTheToken(t *testing.T) {
	var tokenCalls, commandCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("sign") == "" || r.Header.Get("client_id") != "test-id" || r.Header.Get("t") != "1000" {
			t.Errorf("headers id %q t %q sign %q", r.Header.Get("client_id"), r.Header.Get("t"), r.Header.Get("sign"))
		}
		switch {
		case r.URL.Path == "/v1.0/token":
			tokenCalls++
			if r.Header.Get("access_token") != "" {
				t.Error("token request carried an access token")
			}
			_, _ = io.WriteString(w, `{"success":true,"result":{"access_token":"tok","expire_time":7200}}`)
		case strings.HasSuffix(r.URL.Path, "/commands"):
			commandCalls++
			if r.Header.Get("access_token") != "tok" {
				t.Errorf("access token %q", r.Header.Get("access_token"))
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"commands":[{"code":"switch_1","value":true}]}` {
				t.Errorf("body %s", body)
			}
			_, _ = io.WriteString(w, `{"success":true,"result":true}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newCloudClient()
	client.http = server.Client()
	client.now = func() time.Time { return time.UnixMilli(1000) }
	client.configure(Developer{AccessID: "test-id", AccessSecret: "test-secret", Endpoint: server.URL})
	client.remember("dev-a", "switch_1")
	if err := client.Set(context.Background(), "dev-a", true); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), "dev-a", true); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 || commandCalls != 2 {
		t.Fatalf("token calls %d command calls %d", tokenCalls, commandCalls)
	}
}

func TestSignRequestIsStable(t *testing.T) {
	got := signRequest("test-id", "test-secret", "", "1000", http.MethodGet, "/v1.0/token?grant_type=1", nil)
	if len(got) != 64 || strings.ToUpper(got) != got {
		t.Fatalf("sign %s", got)
	}
}

func TestLocalFrameRoundTrip(t *testing.T) {
	key := "0123456789abcdef"
	frame, err := encodeFrame("3.3", cmdControl, key, map[string]any{
		"devId": "dev-a",
		"dps":   map[string]any{"1": true},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePayload("3.3", key, frame)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		DevID string         `json:"devId"`
		DPS   map[string]any `json:"dps"`
	}
	if err := json.Unmarshal(decoded, &body); err != nil {
		t.Fatalf("decode %q: %v", decoded, err)
	}
	if body.DevID != "dev-a" || body.DPS["1"] != true {
		t.Fatalf("body %#v", body)
	}

	frame, err = encodeFrame("3.4", cmdControl, key, map[string]any{"devId": "dev-a", "dps": map[string]any{"20": false}}, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = decodePayload("3.4", key, frame)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), `"devId":"dev-a"`) {
		t.Fatalf("3.4 payload %s", decoded)
	}
}

func TestPlaceholderLocalKeyIsSkipped(t *testing.T) {
	if localReady(Switch{IP: "192.168.1.61", LocalKey: "REPLACE_LOCAL_KEY"}) {
		t.Fatal("placeholder key looked ready")
	}
	if !localReady(Switch{IP: "192.168.1.61", LocalKey: "0123456789abcdef"}) {
		t.Fatal("real key was skipped")
	}
	client := newDeviceClient()
	client.Use(&File{Control: Control{PreferLocal: true, CloudFallback: false}})
	err := client.Set(context.Background(), Switch{
		ID: "a", DeviceID: "dev-a", IP: "192.0.2.61", LocalKey: "REPLACE_LOCAL_KEY",
	}, true)
	if err == nil || !strings.Contains(err.Error(), "local key") {
		t.Fatal(err)
	}
}
