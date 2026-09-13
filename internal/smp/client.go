// Package smp starts and stops the Extron SMP 351's RTMP streams.
//
// Everything the unit needs to know about where a stream goes is configured on
// the unit itself. This server only presses its buttons, over the same web API
// the SMP's own Encoding & Layout page uses.
package smp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"streaming/internal/config"
)

// The unit has two encoders, each holding its own saved RTMP destination:
// Archive carries Mandarin and Confidence carries English. Which is which is
// set up on the SMP, so nothing here recalls or edits a preset.
const (
	archiveChannel    = 1
	confidenceChannel = 3
)

const (
	connectTimeout  = 10 * time.Second
	headerTimeout   = 5 * time.Second
	requestTimeout  = 10 * time.Second
	actionTimeout   = 30 * time.Second
	webProbeTimeout = 2 * time.Second
	// The unit's answers are small; only its preview is long.
	maxBodySize = 1 << 20
)

// publishConfirmWait is how long a start waits for the unit to report that it
// has reached its destination. Measured on firmware 2.11, a working push
// resolves and connects in under three seconds. They are variables so tests
// need not wait.
var (
	publishConfirmWait  = 8 * time.Second
	publishPollInterval = 500 * time.Millisecond
)

type ActiveStream string

const (
	ActiveNone     ActiveStream = "none"
	ActiveEnglish  ActiveStream = "english"
	ActiveMandarin ActiveStream = "mandarin"
)

type State struct {
	ActiveStream     ActiveStream `json:"activeStream"`
	Streaming        bool         `json:"streaming"`
	StatusMessage    string       `json:"statusMessage"`
	LastError        *string      `json:"lastError"`
	SmpReachable     bool         `json:"smpReachable"`
	QueriedAtEpochMs int64        `json:"queriedAtEpochMs"`
}

type Client struct {
	// One button press at a time. A start reads both encoders, may switch one
	// off, and then waits for the other to connect; a poll arriving in the
	// middle of that would report a state that has already moved on.
	mu        sync.Mutex
	api       *http.Client
	streaming *http.Client
}

func New() *Client {
	transport := &http.Transport{
		// The SMP presents its own self-signed certificate, which no
		// authority will ever vouch for. The alternative is plain HTTP to the
		// same unit over the same wire, so this is the better of the two.
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: headerTimeout,
		DialContext:           (&net.Dialer{Timeout: connectTimeout}).DialContext,
	}
	return &Client{
		api: &http.Client{Transport: transport, Timeout: requestTimeout},
		// The preview has no overall timeout: it is an endless fragmented MP4,
		// relayed for as long as the browser keeps watching.
		streaming: &http.Client{Transport: transport},
	}
}

// QueryState reads which language, if any, is going out. The second result is
// false when a button press is already in flight, so a poll steps aside rather
// than queueing behind it and reporting a reading that is by then stale.
func (c *Client) QueryState(cfg config.Config) (State, bool) {
	if !cfg.IsSmpConfigured() {
		return notConfigured(), true
	}
	if !c.mu.TryLock() {
		return State{}, false
	}
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	live, err := c.publishing(ctx, cfg)
	if err != nil {
		return failed("SMP is not reachable", err), true
	}
	return stateFrom(live), true
}

func (c *Client) StartEnglish(cfg config.Config) State {
	return c.start(cfg, ActiveEnglish)
}

func (c *Client) StartMandarin(cfg config.Config) State {
	return c.start(cfg, ActiveMandarin)
}

func (c *Client) start(cfg config.Config, wanted ActiveStream) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	if err := c.startPublish(ctx, cfg, wanted); err != nil {
		return failed("Start failed", err)
	}
	// Read back rather than assume: what the unit reports is what the page
	// shows either way a moment later.
	live, err := c.publishing(ctx, cfg)
	if err != nil {
		return failed("Start failed", err)
	}
	return stateFrom(live)
}

func (c *Client) startPublish(ctx context.Context, cfg config.Config, wanted ActiveStream) error {
	wantedChannel := channelFor(wanted)
	if wantedChannel == 0 {
		return fmt.Errorf("unknown language %q", wanted)
	}

	live, err := c.publishing(ctx, cfg)
	if err != nil {
		return err
	}
	// Only one language goes out at a time. Both encoders are read first, so
	// one that is already idle is left completely alone, and a language that
	// is already live is not interrupted and restarted.
	if other := otherChannel(wantedChannel); live[other] {
		if err := c.setPublish(ctx, cfg, other, false); err != nil {
			return err
		}
	}
	if live[wantedChannel] {
		return nil
	}
	// The unit refuses a publish on an encoder that is not running, answering
	// E13, so these two writes together are what its own START RTMP STREAM
	// button amounts to.
	if err := c.enableEncoder(ctx, cfg, wantedChannel); err != nil {
		return err
	}
	if err := c.setPublish(ctx, cfg, wantedChannel, true); err != nil {
		return err
	}
	return c.confirmPublish(ctx, cfg, wantedChannel)
}

// Stop ends whichever language is going out. The encoders themselves are left
// running: switching one off is what makes the unit report its configuration
// as modified and grey out half of its own streaming page.
func (c *Client) Stop(cfg config.Config) State {
	if !cfg.IsSmpConfigured() {
		return notConfigured()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	live, err := c.publishing(ctx, cfg)
	if err != nil {
		return failed("Stop failed", err)
	}
	// Nothing is sent to an encoder that is already idle.
	for _, channel := range []int{archiveChannel, confidenceChannel} {
		if !live[channel] {
			continue
		}
		if err := c.setPublish(ctx, cfg, channel, false); err != nil {
			return failed("Stop failed", err)
		}
	}
	live, err = c.publishing(ctx, cfg)
	if err != nil {
		return failed("Stop failed", err)
	}
	return stateFrom(live)
}

// publishing reports which encoders are pushing to their destinations. Both
// are asked for in one request, which the unit answers in order.
func (c *Client) publishing(ctx context.Context, cfg config.Config) (map[int]bool, error) {
	channels := []int{archiveChannel, confidenceChannel}
	uris := make([]string, 0, len(channels))
	for _, channel := range channels {
		uris = append(uris, publishURI(channel))
	}
	values, err := c.read(ctx, cfg, uris...)
	if err != nil {
		return nil, err
	}
	live := make(map[int]bool, len(channels))
	for _, channel := range channels {
		flag, err := values.number(publishURI(channel))
		if err != nil {
			return nil, err
		}
		live[channel] = flag == 1
	}
	return live, nil
}

func (c *Client) setPublish(ctx context.Context, cfg config.Config, channel int, on bool) error {
	verb := "stop"
	if on {
		verb = "start"
	}
	if err := c.write(ctx, cfg, publishURI(channel), boolValue(on)); err != nil {
		return fmt.Errorf("%s the %s stream: %w", verb, channelName(channel), err)
	}
	return nil
}

func (c *Client) enableEncoder(ctx context.Context, cfg config.Config, channel int) error {
	if err := c.write(ctx, cfg, streamEnableURI(channel), boolValue(true)); err != nil {
		return fmt.Errorf("enable the %s encoder: %w", channelName(channel), err)
	}
	return nil
}

// confirmPublish waits until the unit reports a destination it has actually
// resolved and connected to. The publish flag is set the moment the command is
// accepted, which is before the destination has been contacted, so a push that
// is going to be refused looks exactly like a working one for a second or two.
func (c *Client) confirmPublish(ctx context.Context, cfg config.Config, channel int) error {
	deadline := time.Now().Add(publishConfirmWait)
	for {
		current, err := c.session(ctx, cfg, channel)
		switch {
		case err != nil:
			return err
		case !current.publishing:
			return fmt.Errorf(
				"the %s stream stopped as soon as it started; check its destination on the SMP",
				channelName(channel),
			)
		case current.destination != "":
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf(
				"the %s stream did not reach its destination within %s; check its URL and stream key on the SMP, and that this network allows RTMP out",
				channelName(channel), publishConfirmWait,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(publishPollInterval):
		}
	}
}

// session is what one RTMP channel reports about its push. Only these two
// things are taken from it: the same resource also carries the destination URL
// and its credentials, which this server has no reason to hold.
type session struct {
	publishing  bool
	destination string
}

func (c *Client) session(ctx context.Context, cfg config.Config, channel int) (session, error) {
	values, err := c.read(ctx, cfg, rtmpURI(channel))
	if err != nil {
		return session{}, err
	}
	var resource struct {
		PubControl  int `json:"pub_control"`
		SessionInfo []struct {
			ResolvedIP   string `json:"resolved_ip"`
			ResolvedPort int    `json:"resolved_port"`
		} `json:"session_info"`
	}
	if err := values.decode(rtmpURI(channel), &resource); err != nil {
		return session{}, err
	}
	current := session{publishing: resource.PubControl == 1}
	// The unit keeps a fixed row of session slots and fills the first one it
	// uses, leaving the rest zeroed.
	for _, info := range resource.SessionInfo {
		if info.ResolvedPort == 0 || info.ResolvedIP == "" || info.ResolvedIP == "0.0.0.0" {
			continue
		}
		current.destination = net.JoinHostPort(info.ResolvedIP, strconv.Itoa(info.ResolvedPort))
		break
	}
	return current, nil
}

// Preview opens the unit's own live preview, a fragmented MP4 that it produces
// whether or not anything is being streamed. The caller closes the body.
func (c *Client) Preview(ctx context.Context, cfg config.Config) (io.ReadCloser, error) {
	// The cache-busting parameter is what the unit's own preview page sends.
	path := fmt.Sprintf("/mp4stream?d=%d", time.Now().UnixMilli())
	req, err := c.request(ctx, cfg, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.streaming.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, errUnauthorized
		}
		return nil, fmt.Errorf("the SMP answered %s for its preview", resp.Status)
	}
	return resp.Body, nil
}

// Stage names the last step of the connection that was reached, so the
// configuration page can say whether the address, the network, the
// credentials, the API, or the preview is at fault.
type Stage string

const (
	StageAddress Stage = "address"
	StageNetwork Stage = "network"
	StageAuth    Stage = "auth"
	StageCommand Stage = "command"
	StagePreview Stage = "preview"
	StageOK      Stage = "ok"
)

type Diagnosis struct {
	OK      bool   `json:"ok"`
	Stage   Stage  `json:"stage"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
	Address string `json:"address"`
	Hint    string `json:"hint"`
}

// Diagnose checks both of the things the control page needs, the API that
// starts a stream and the preview that shows the picture, and attributes a
// failure to the layer that caused it. It only reads, and it never touches
// stored configuration.
func (c *Client) Diagnose(cfg config.Config) Diagnosis {
	c.mu.Lock()
	defer c.mu.Unlock()

	if strings.TrimSpace(cfg.SmpHost) == "" {
		return Diagnosis{Stage: StageAddress, Summary: "Enter the SMP address first."}
	}
	if strings.TrimSpace(cfg.SmpUsername) == "" {
		return Diagnosis{Stage: StageAddress, Summary: "Enter the SMP username first."}
	}
	addr := address(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	live, err := c.publishing(ctx, cfg)
	if err != nil {
		return apiDiagnosis(cfg, addr, err)
	}

	preview, err := c.Preview(ctx, cfg)
	if err != nil {
		return Diagnosis{
			Stage:   StagePreview,
			Address: addr,
			Summary: "The SMP answered its configuration API, but not its live preview.",
			Detail:  err.Error(),
			Hint: "The buttons will still work. Only the picture on the control page is affected, so check that the " +
				"preview is switched on under the SMP's own Configuration, Encoding & Layout page.",
		}
	}
	_ = preview.Close()

	return Diagnosis{
		OK:      true,
		Stage:   StageOK,
		Address: addr,
		Summary: addr + " answered on both its configuration API and its live preview.",
		Detail: fmt.Sprintf("Mandarin (Archive) is %s, English (Confidence) is %s.",
			streamWord(live[archiveChannel]), streamWord(live[confidenceChannel])),
	}
}

func apiDiagnosis(cfg config.Config, addr string, err error) Diagnosis {
	result := Diagnosis{Stage: StageCommand, Address: addr, Detail: err.Error()}

	var dnsError *net.DNSError
	var netError net.Error
	var recordError tls.RecordHeaderError
	switch {
	case errors.Is(err, errUnauthorized):
		result.Stage = StageAuth
		result.Summary = "Reached " + addr + ", but the username or password was rejected."
		result.Hint = "The network path is fine. These are the same credentials as the SMP's own web page, and a blank " +
			"password box keeps the previously saved one."
	case errors.As(err, &dnsError):
		result.Stage = StageAddress
		result.Summary = "The SMP host name could not be resolved."
		result.Hint = "Use the SMP's IP address instead of a name, or check this device's DNS settings."
	case errors.As(err, &netError) && netError.Timeout():
		result.Stage = StageNetwork
		result.Summary = "No reply from " + addr + "."
		result.Hint = networkHint(cfg)
	case errors.Is(err, syscall.ECONNREFUSED):
		result.Stage = StageNetwork
		result.Summary = addr + " refused the connection."
		result.Hint = networkHint(cfg)
	case errors.Is(err, syscall.ECONNRESET):
		result.Stage = StageNetwork
		result.Summary = "The connection to " + addr + " was cut off before anything was exchanged."
		result.Hint = networkHint(cfg)
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		result.Stage = StageNetwork
		result.Summary = "This device has no route to " + addr + "."
		result.Hint = "Compare this device's IP address and subnet with the SMP's."
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &recordError):
		// Every port but 80 is spoken to over TLS, so a plain HTTP service on
		// an unusual port lands here. Which of the two errors it is depends on
		// whether the service answers the handshake bytes or rejects them.
		result.Stage = StageNetwork
		result.Summary = "Something answered on " + addr + ", but not with HTTPS."
		result.Hint = "If the SMP's web interface is served without HTTPS, put port 80 in the address."
	default:
		result.Summary = "Reached " + addr + ", but its answer could not be used."
	}
	return result
}

// networkHint separates "nothing here can reach the unit" from "only the
// configured port is closed", which are fixed in different places.
func networkHint(cfg config.Config) string {
	port, ok := reachablePort(cfg.SmpHost)
	switch {
	case ok && port != cfg.SmpPort:
		return fmt.Sprintf(
			"The SMP does answer on port %d from this device, so put that port in the address instead of %d.",
			port, cfg.SmpPort,
		)
	case ok:
		return "The port itself accepts connections, so the unit is reachable but did not answer in time. Try again."
	default:
		return "The SMP answers on neither of its web ports from this device, so nothing here can reach the unit. " +
			"Compare this device's IP address and subnet with the SMP's."
	}
}

func reachablePort(host string) (int, bool) {
	for _, port := range []int{443, 80} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), webProbeTimeout)
		if err == nil {
			_ = conn.Close()
			return port, true
		}
	}
	return 0, false
}

// resourcePath is the unit's configuration API. A GET takes one uri parameter
// per resource wanted; a PUT takes a list of resources and their new values.
const resourcePath = "/api/swis/resources"

func (c *Client) read(ctx context.Context, cfg config.Config, uris ...string) (resources, error) {
	query := make(url.Values, 1)
	for _, uri := range uris {
		query.Add("uri", uri)
	}
	req, err := c.request(ctx, cfg, http.MethodGet, resourcePath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return c.exchange(req, uris)
}

func (c *Client) write(ctx context.Context, cfg config.Config, uri string, value any) error {
	body, err := json.Marshal([]map[string]any{{"uri": uri, "value": value}})
	if err != nil {
		return err
	}
	req, err := c.request(ctx, cfg, http.MethodPut, resourcePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = c.exchange(req, nil)
	return err
}

// exchange sends one API call and turns the answer into either values or an
// error. Which of the two it is lives in the body rather than the status code:
// the unit answers a command it has refused with HTTP 200 and an Extron error
// code beside the resource it refused.
func (c *Client) exchange(req *http.Request, wanted []string) (resources, error) {
	resp, err := c.api.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the SMP answered %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("read the SMP's answer: %w", err)
	}
	var entries []struct {
		Meta struct {
			URI         string `json:"uri"`
			Status      string `json:"status"`
			Description string `json:"description"`
		} `json:"meta"`
		Result json.RawMessage `json:"result"`
	}
	// The body is never quoted in an error: it can carry the destination URL
	// and its stream key.
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("the SMP's answer was not in the expected form")
	}

	values := make(resources, len(entries))
	for _, entry := range entries {
		if entry.Meta.Status != "" {
			return nil, refusalError(entry.Meta.URI, entry.Meta.Status, entry.Meta.Description)
		}
		values[entry.Meta.URI] = entry.Result
	}
	for _, uri := range wanted {
		if _, ok := values[uri]; !ok {
			return nil, fmt.Errorf("the SMP did not answer for %s", uri)
		}
	}
	return values, nil
}

// refusalError names the resource as well as the code, because one call can
// carry several and the code alone does not say which was refused.
func refusalError(uri, status, description string) error {
	if description != "" {
		return fmt.Errorf("the SMP refused %s: %s (%s)", uri, description, status)
	}
	return fmt.Errorf("the SMP refused %s with %s", uri, status)
}

// resources holds one answer per resource, keyed by its uri.
type resources map[string]json.RawMessage

func (r resources) number(uri string) (int, error) {
	var value int
	if err := r.decode(uri, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func (r resources) decode(uri string, into any) error {
	raw, ok := r[uri]
	if !ok {
		return fmt.Errorf("the SMP did not answer for %s", uri)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("could not read %s from the SMP's answer", uri)
	}
	return nil
}

func (c *Client) request(
	ctx context.Context,
	cfg config.Config,
	method, path string,
	body io.Reader,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint(cfg, path), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(strings.TrimSpace(cfg.SmpUsername), cfg.SmpPassword)
	return req, nil
}

// endpoint takes its scheme from the port, so a unit whose web interface is
// reachable only over plain HTTP still works: the API and the preview are
// served identically either way.
func endpoint(cfg config.Config, path string) string {
	scheme := "https"
	if cfg.SmpPort == 80 {
		scheme = "http"
	}
	return scheme + "://" + address(cfg) + path
}

// address keeps IPv6 hosts usable: ParseHostPort stores them without brackets,
// which a plain host:port concatenation would mangle.
func address(cfg config.Config) string {
	return net.JoinHostPort(cfg.SmpHost, strconv.Itoa(cfg.SmpPort))
}

var errUnauthorized = errors.New("the SMP rejected the username or password")

// transportError keeps the useful part of a failed request. http.Client wraps
// every failure in a *url.Error that repeats the whole address, which reads
// badly in the status line under the buttons.
func transportError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) && urlError.Err != nil {
		return urlError.Err
	}
	return err
}

func stateFrom(live map[int]bool) State {
	state := State{
		ActiveStream:     ActiveNone,
		Streaming:        live[archiveChannel] || live[confidenceChannel],
		StatusMessage:    "Idle",
		SmpReachable:     true,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
	switch {
	case live[archiveChannel] && live[confidenceChannel]:
		// Not something these buttons can produce, but somebody working on the
		// unit's own page can, and saying so is more use than naming one.
		state.StatusMessage = "Streaming Mandarin and English"
	case live[archiveChannel]:
		state.ActiveStream = ActiveMandarin
		state.StatusMessage = "Streaming Mandarin"
	case live[confidenceChannel]:
		state.ActiveStream = ActiveEnglish
		state.StatusMessage = "Streaming English"
	}
	return state
}

func channelFor(stream ActiveStream) int {
	switch stream {
	case ActiveMandarin:
		return archiveChannel
	case ActiveEnglish:
		return confidenceChannel
	default:
		return 0
	}
}

func otherChannel(channel int) int {
	if channel == archiveChannel {
		return confidenceChannel
	}
	return archiveChannel
}

// channelName uses the unit's own words for its encoders, so a message here
// points at something the operator can find on the SMP's page.
func channelName(channel int) string {
	if channel == confidenceChannel {
		return "Confidence"
	}
	return "Archive"
}

func publishURI(channel int) string {
	return fmt.Sprintf("/streamer/rtmp/%d/pub_control", channel)
}

func rtmpURI(channel int) string {
	return fmt.Sprintf("/streamer/rtmp/%d", channel)
}

func streamEnableURI(channel int) string {
	return fmt.Sprintf("/encoder/%d/stream_enable", channel)
}

// boolValue matches what the unit accepts: a JSON number. It answers E13 to
// both true and "1".
func boolValue(on bool) int {
	if on {
		return 1
	}
	return 0
}

func streamWord(live bool) string {
	if live {
		return "streaming"
	}
	return "idle"
}

func notConfigured() State {
	msg := "Configure the SMP address and credentials on the configuration page."
	return State{
		ActiveStream:     ActiveNone,
		StatusMessage:    "SMP not configured",
		LastError:        &msg,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}

func failed(message string, err error) State {
	detail := err.Error()
	return State{
		ActiveStream:     ActiveNone,
		StatusMessage:    message,
		LastError:        &detail,
		QueriedAtEpochMs: time.Now().UnixMilli(),
	}
}
