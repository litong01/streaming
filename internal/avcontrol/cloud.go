package avcontrol

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultCloudEndpoint = "https://openapi.tuyaus.com"

type cloudClient struct {
	mu       sync.Mutex
	endpoint string
	clientID string
	secret   string
	http     *http.Client
	token    string
	expiry   time.Time
	codes    map[string]string
	now      func() time.Time
}

func newCloudClient() *cloudClient {
	return &cloudClient{
		http:  &http.Client{Timeout: 20 * time.Second},
		codes: map[string]string{},
		now:   time.Now,
	}
}

func (c *cloudClient) configure(dev Developer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	endpoint := strings.TrimRight(strings.TrimSpace(dev.Endpoint), "/")
	if endpoint == "" {
		endpoint = defaultCloudEndpoint
	}
	if endpoint != c.endpoint || dev.AccessID != c.clientID || dev.AccessSecret != c.secret {
		c.token = ""
		c.expiry = time.Time{}
		c.codes = map[string]string{}
	}
	c.endpoint = endpoint
	c.clientID = dev.AccessID
	c.secret = dev.AccessSecret
}

func (c *cloudClient) ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientID != "" && c.secret != ""
}

func (c *cloudClient) Set(ctx context.Context, deviceID string, on bool) error {
	if !c.ready() {
		return fmt.Errorf("tuya cloud is not configured")
	}
	code, err := c.switchCode(ctx, deviceID)
	if err != nil {
		var last error
		for _, candidate := range []string{"switch_1", "switch_led", "switch"} {
			last = c.command(ctx, deviceID, candidate, on)
			if last == nil {
				c.remember(deviceID, candidate)
				return nil
			}
		}
		if err != nil && last != nil {
			return last
		}
		return err
	}
	return c.command(ctx, deviceID, code, on)
}

func (c *cloudClient) readSwitch(ctx context.Context, deviceID string) (bool, error) {
	if !c.ready() {
		return false, fmt.Errorf("tuya cloud is not configured")
	}
	code, err := c.switchCode(ctx, deviceID)
	if err != nil {
		return false, err
	}
	status, err := c.status(ctx, deviceID)
	if err != nil {
		return false, err
	}
	for _, item := range status {
		if item.Code != code {
			continue
		}
		on, ok := asBool(item.Value)
		if !ok {
			return false, fmt.Errorf("switch state is not on or off")
		}
		return on, nil
	}
	return false, fmt.Errorf("switch state missing")
}

func (c *cloudClient) switchCode(ctx context.Context, deviceID string) (string, error) {
	c.mu.Lock()
	if code := c.codes[deviceID]; code != "" {
		c.mu.Unlock()
		return code, nil
	}
	c.mu.Unlock()

	var result struct {
		Functions []struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"functions"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1.0/devices/"+url.PathEscape(deviceID)+"/functions", nil, true, &result); err != nil {
		return "", err
	}
	code := chooseSwitch(result.Functions)
	if code == "" {
		return "", fmt.Errorf("tuya device %s has no switch", deviceID)
	}
	c.remember(deviceID, code)
	return code, nil
}

func chooseSwitch(functions []struct {
	Code string `json:"code"`
	Type string `json:"type"`
}) string {
	byCode := map[string]string{}
	var bools []string
	for _, fn := range functions {
		code := strings.TrimSpace(fn.Code)
		if code == "" {
			continue
		}
		byCode[code] = code
		if strings.EqualFold(fn.Type, "Boolean") && strings.Contains(strings.ToLower(code), "switch") {
			bools = append(bools, code)
		}
	}
	for _, preferred := range []string{"switch_1", "switch_led", "switch"} {
		if code, ok := byCode[preferred]; ok {
			return code
		}
	}
	if len(bools) > 0 {
		return bools[0]
	}
	return ""
}

func (c *cloudClient) remember(deviceID, code string) {
	c.mu.Lock()
	c.codes[deviceID] = code
	c.mu.Unlock()
}

func (c *cloudClient) command(ctx context.Context, deviceID, code string, on bool) error {
	body := map[string]any{
		"commands": []map[string]any{
			{"code": code, "value": on},
		},
	}
	var result any
	return c.call(ctx, http.MethodPost, "/v1.0/devices/"+url.PathEscape(deviceID)+"/commands", body, true, &result)
}

type statusItem struct {
	Code  string `json:"code"`
	Value any    `json:"value"`
}

func (c *cloudClient) status(ctx context.Context, deviceID string) ([]statusItem, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodGet, "/v1.0/devices/"+url.PathEscape(deviceID)+"/status", nil, true, &raw); err != nil {
		return nil, err
	}
	var items []statusItem
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var wrapped struct {
		Status []statusItem `json:"status"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("tuya status")
	}
	return wrapped.Status, nil
}

func (c *cloudClient) call(ctx context.Context, method, path string, body any, withToken bool, into any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	token := ""
	if withToken {
		var err error
		token, err = c.accessToken(ctx)
		if err != nil {
			return err
		}
	}
	stamp := strconv.FormatInt(c.now().UnixMilli(), 10)
	c.mu.Lock()
	clientID, secret, endpoint := c.clientID, c.secret, c.endpoint
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("client_id", clientID)
	req.Header.Set("sign_method", "HMAC-SHA256")
	req.Header.Set("t", stamp)
	req.Header.Set("sign", signRequest(clientID, secret, token, stamp, method, path, payload))
	if token != "" {
		req.Header.Set("access_token", token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tuya cloud: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("tuya cloud: %w", err)
	}
	var envelope struct {
		Success bool            `json:"success"`
		Code    int             `json:"code"`
		Msg     string          `json:"msg"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("tuya cloud returned %d", resp.StatusCode)
	}
	if !envelope.Success {
		msg := strings.TrimSpace(envelope.Msg)
		if msg == "" {
			msg = "request failed"
		}
		return fmt.Errorf("tuya cloud: %s", msg)
	}
	if into == nil || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, into); err != nil {
		return fmt.Errorf("tuya cloud result")
	}
	return nil
}

func (c *cloudClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && c.now().Before(c.expiry) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	var result struct {
		AccessToken string `json:"access_token"`
		ExpireTime  int    `json:"expire_time"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1.0/token?grant_type=1", nil, false, &result); err != nil {
		return "", err
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("tuya cloud: empty token")
	}
	ttl := result.ExpireTime
	if ttl <= 0 {
		ttl = 3600
	}
	if ttl > 120 {
		ttl -= 60
	}
	c.mu.Lock()
	c.token = result.AccessToken
	c.expiry = c.now().Add(time.Duration(ttl) * time.Second)
	token := c.token
	c.mu.Unlock()
	return token, nil
}

func signRequest(clientID, secret, token, stamp, method, path string, body []byte) string {
	sum := sha256.Sum256(body)
	content := hex.EncodeToString(sum[:])
	stringToSign := method + "\n" + content + "\n" + "\n" + path
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(clientID + token + stamp + stringToSign))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

func asBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case float64:
		return typed != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "on", "1":
			return true, true
		case "false", "off", "0":
			return false, true
		}
	}
	return false, false
}
