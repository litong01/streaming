package avcontrol

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrSaveFailed marks a configuration that was valid but could not be stored.
var ErrSaveFailed = errors.New("save av control config")

// ItemView is one controllable item, in power-on order. The local key stays
// on the device; the page only learns whether one is already saved.
type ItemView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	DeviceID    string `json:"deviceId"`
	IP          string `json:"ip"`
	HasLocalKey bool   `json:"hasLocalKey"`
	Protocol    string `json:"protocol"`
	DelaySec    int    `json:"delaySec"`
}

// DayView is one weekday. Empty On or Off means that side is not scheduled.
type DayView struct {
	ID  string `json:"id"`
	On  string `json:"on"`
	Off string `json:"off"`
}

// ConfigView is the AV configuration page. Secrets are not included.
type ConfigView struct {
	ProjectCode     string     `json:"projectCode"`
	AccessID        string     `json:"accessId"`
	HasAccessSecret bool       `json:"hasAccessSecret"`
	Items           []ItemView `json:"items"`
	Days            []DayView  `json:"days"`
	EnsureOff       string     `json:"ensureOff"`
	DelayMinutes    int        `json:"delayMinutes"`
}

// ItemEdit is one row from the AV configuration page.
type ItemEdit struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	DeviceID string `json:"deviceId"`
	IP       string `json:"ip"`
	LocalKey string `json:"localKey"`
	Protocol string `json:"protocol"`
	DelaySec int    `json:"delaySec"`
}

// DayEdit is one weekday from the schedule list. An empty time means none.
type DayEdit struct {
	ID  string `json:"id"`
	On  string `json:"on"`
	Off string `json:"off"`
}

// ConfigEdit is a save of the AV configuration page. An empty local key or
// access secret keeps the value already stored for that item. Omitted
// schedule fields keep the schedule already stored.
type ConfigEdit struct {
	ProjectCode  string     `json:"projectCode"`
	AccessID     string     `json:"accessId"`
	AccessSecret string     `json:"accessSecret"`
	Items        []ItemEdit `json:"items"`
	Days         []DayEdit  `json:"days,omitempty"`
	EnsureOff    *string    `json:"ensureOff,omitempty"`
	DelayMinutes *int       `json:"delayMinutes,omitempty"`
}

var dayIDs = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// Configuration returns the page model. A missing saved file yields the seed.
func (c *Controller) Configuration() (ConfigView, error) {
	c.reload()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		if c.problem != "" {
			return ConfigView{}, errors.New(c.problem)
		}
		return ConfigView{}, ErrNotConfigured
	}
	return c.file.editorView(), nil
}

// SaveConfiguration writes avcontrol.yaml. The seed is left behind once that
// file exists. Guards and every section this page does not edit stay as they were.
func (c *Controller) SaveConfiguration(edit ConfigEdit) (ConfigView, error) {
	c.reload()
	c.mu.Lock()
	file := c.file
	source := append([]byte(nil), c.source...)
	path := c.path
	c.mu.Unlock()
	if file == nil {
		if c.problemText() != "" {
			return ConfigView{}, errors.New(c.problemText())
		}
		return ConfigView{}, ErrNotConfigured
	}

	nextBytes, next, err := applyEdit(source, file, edit)
	if err != nil {
		return ConfigView{}, err
	}
	if err := writeYAML(path, nextBytes); err != nil {
		return ConfigView{}, fmt.Errorf("%w: %v", ErrSaveFailed, err)
	}
	info, statErr := os.Stat(path)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.source = nextBytes
	c.fromSeed = false
	if statErr == nil {
		c.mod = info.ModTime()
	}
	c.applyLocked(next)
	return next.editorView(), nil
}

func (f *File) editorView() ConfigView {
	delay := make(map[string]int, len(f.Startup))
	order := make([]string, 0, len(f.Switches))
	seen := make(map[string]bool, len(f.Switches))
	for _, step := range f.Startup {
		if step.Switch == "" || seen[step.Switch] {
			continue
		}
		seen[step.Switch] = true
		delay[step.Switch] = step.SettleSec
		order = append(order, step.Switch)
	}
	byID := make(map[string]Switch, len(f.Switches))
	for _, sw := range f.Switches {
		byID[sw.ID] = sw
		if seen[sw.ID] {
			continue
		}
		seen[sw.ID] = true
		order = append(order, sw.ID)
	}

	items := make([]ItemView, 0, len(order))
	for _, id := range order {
		sw, ok := byID[id]
		if !ok {
			continue
		}
		protocol := sw.Protocol
		if protocol == "" {
			protocol = "3.4"
		}
		items = append(items, ItemView{
			ID:          sw.ID,
			Label:       sw.Label,
			DeviceID:    sw.DeviceID,
			IP:          sw.IP,
			HasLocalKey: storedSecret(sw.LocalKey) != "",
			Protocol:    protocol,
			DelaySec:    delay[id],
		})
	}
	days := make([]DayView, len(dayIDs))
	for i, id := range dayIDs {
		day := f.Schedule.Day(time.Weekday(i))
		days[i] = DayView{ID: id, On: clockText(day.On), Off: clockText(day.Off)}
	}
	minutes := f.OffDelay.StepMinutes
	if minutes <= 0 {
		minutes = 60
	}
	return ConfigView{
		ProjectCode:     f.Developer.ProjectCode,
		AccessID:        f.Developer.AccessID,
		HasAccessSecret: storedSecret(f.Developer.AccessSecret) != "",
		Items:           items,
		Days:            days,
		EnsureOff:       clockText(optionalClock(f.Schedule.EnsureOffDaily)),
		DelayMinutes:    minutes,
	}
}

func applyEdit(source []byte, current *File, edit ConfigEdit) ([]byte, *File, error) {
	if len(edit.Items) == 0 {
		return nil, nil, errors.New("add at least one item")
	}
	byID := make(map[string]Switch, len(current.Switches))
	for _, sw := range current.Switches {
		byID[sw.ID] = sw
	}

	switches := make([]Switch, 0, len(edit.Items))
	steps := make([]Step, 0, len(edit.Items))
	used := make(map[string]bool, len(edit.Items))
	deviceOwner := make(map[string]string, len(edit.Items))
	for i, item := range edit.Items {
		label := strings.TrimSpace(item.Label)
		if label == "" {
			return nil, nil, fmt.Errorf("item %d needs a name", i+1)
		}
		deviceID := strings.TrimSpace(item.DeviceID)
		if deviceID == "" {
			return nil, nil, fmt.Errorf("%s needs a device id", label)
		}
		if item.DelaySec < 0 {
			return nil, nil, fmt.Errorf("%s has a negative delay", label)
		}
		ip, err := ipFor(item.IP)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", label, err)
		}

		var sw Switch
		id := strings.TrimSpace(item.ID)
		if prev, ok := byID[id]; ok && id != "" {
			if used[id] {
				return nil, nil, fmt.Errorf("%s is listed twice", label)
			}
			sw = prev
		}
		protocol, err := protocolFor(sw.Protocol, item.Protocol)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", label, err)
		}
		key, err := localKeyFor(sw, deviceID, ip, item.LocalKey)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", label, err)
		}
		sw.Label = label
		sw.DeviceID = deviceID
		sw.IP = ip
		sw.Protocol = protocol
		sw.LocalKey = key
		if sw.PowerRestore == "" {
			sw.PowerRestore = "off"
		}
		if sw.ID == "" {
			sw.ID = uniqueID(label, used)
		}
		used[sw.ID] = true
		if other := deviceOwner[deviceID]; other != "" {
			return nil, nil, fmt.Errorf("%s uses the same device id as %s", label, other)
		}
		deviceOwner[deviceID] = label
		switches = append(switches, sw)
		steps = append(steps, Step{Switch: sw.ID, SettleSec: item.DelaySec})
	}
	steps = placeWakeMacs(current.Startup, steps)

	schedule, minutes, err := applySchedule(current, edit)
	if err != nil {
		return nil, nil, err
	}

	next := *current
	next.Switches = switches
	next.Startup = steps
	next.Schedule = schedule
	next.OffDelay.StepMinutes = minutes
	secret, err := accessSecretFor(current.Developer, edit)
	if err != nil {
		return nil, nil, err
	}
	next.Developer.ProjectCode = strings.TrimSpace(edit.ProjectCode)
	next.Developer.AccessID = strings.TrimSpace(edit.AccessID)
	next.Developer.AccessSecret = secret
	next.location = nil
	if err := next.validate(); err != nil {
		return nil, nil, err
	}

	out, err := rewriteDocument(source, &next, edit.Days != nil, edit.DelayMinutes != nil)
	if err != nil {
		return nil, nil, err
	}
	return out, &next, nil
}

// placeWakeMacs keeps a wake step attached to the switch it followed. Macs
// are not edited on this page, and dropping the step would skip the wake.
func placeWakeMacs(original, switches []Step) []Step {
	type held struct {
		after string
		step  Step
	}
	var holds []held
	prev := ""
	for _, step := range original {
		if step.WakeMacs && step.Switch == "" {
			holds = append(holds, held{after: prev, step: step})
			continue
		}
		if step.Switch != "" {
			prev = step.Switch
		}
	}
	if len(holds) == 0 {
		return switches
	}

	present := make(map[string]bool, len(switches))
	for _, step := range switches {
		present[step.Switch] = true
	}
	out := make([]Step, 0, len(switches)+len(holds))
	for _, hold := range holds {
		if hold.after == "" {
			out = append(out, hold.step)
		}
	}
	for _, step := range switches {
		out = append(out, step)
		for _, hold := range holds {
			if hold.after == step.Switch {
				out = append(out, hold.step)
			}
		}
	}
	for _, hold := range holds {
		if hold.after != "" && !present[hold.after] {
			out = append(out, hold.step)
		}
	}
	return out
}

func uniqueID(label string, used map[string]bool) string {
	base := slug(label)
	if base == "" {
		base = "item"
	}
	id := base
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// storedSecret drops the seed placeholders. An empty box then asks for a
// real key instead of saving REPLACE_LOCAL_KEY as though it were one.
// localKeyFor keeps a saved key only for the same device. Filling in an IP
// that was empty is allowed, because that key has not been aimed at an
// address yet. A different device id, or a different address, requires the
// key to be entered again so it is never used against another target.
func localKeyFor(prev Switch, deviceID, ip, typed string) (string, error) {
	if key := strings.TrimSpace(typed); key != "" {
		return key, nil
	}
	if prev.ID == "" {
		return "", errNeedsLocalKey
	}
	saved := storedSecret(prev.LocalKey)
	if saved == "" {
		return "", errNeedsLocalKey
	}
	if strings.TrimSpace(prev.DeviceID) != strings.TrimSpace(deviceID) {
		return "", errRetargetKey
	}
	savedIP := strings.TrimSpace(prev.IP)
	if savedIP != "" && savedIP != strings.TrimSpace(ip) {
		return "", errRetargetKey
	}
	return saved, nil
}

func accessSecretFor(current Developer, edit ConfigEdit) (string, error) {
	if secret := strings.TrimSpace(edit.AccessSecret); secret != "" {
		return secret, nil
	}
	saved := storedSecret(current.AccessSecret)
	savedID := strings.TrimSpace(current.AccessID)
	nextID := strings.TrimSpace(edit.AccessID)
	if saved != "" && savedID != "" && savedID != nextID {
		return "", errRetargetSecret
	}
	return saved, nil
}

func ipFor(value string) (string, error) {
	ip := strings.TrimSpace(value)
	if ip == "" {
		return "", nil
	}
	if net.ParseIP(ip) == nil {
		return "", errBadIP
	}
	return ip, nil
}

func protocolFor(existing, typed string) (string, error) {
	protocol := strings.TrimSpace(typed)
	if protocol == "" {
		protocol = strings.TrimSpace(existing)
	}
	if protocol == "" {
		protocol = "3.4"
	}
	switch protocol {
	case "3.3", "3.4", "3.5":
		return protocol, nil
	default:
		return "", errBadProtocol
	}
}

var (
	errNeedsLocalKey  = errors.New("needs a local key")
	errRetargetKey    = errors.New("enter the local key again for the new address")
	errRetargetSecret = errors.New("enter the access secret again for the new access id")
	errBadIP          = errors.New("needs a valid IP address")
	errBadProtocol    = errors.New("protocol must be 3.3, 3.4, or 3.5")
)

func storedSecret(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "", "REPLACE_LOCAL_KEY", "1234567890":
		return ""
	default:
		return value
	}
}

func slug(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func applySchedule(current *File, edit ConfigEdit) (Schedule, int, error) {
	schedule := current.Schedule
	minutes := current.OffDelay.StepMinutes
	if minutes <= 0 {
		minutes = 60
	}
	if edit.Days != nil {
		if len(edit.Days) != len(dayIDs) {
			return Schedule{}, 0, errors.New("schedule needs all seven days")
		}
		seen := make(map[string]Day, len(dayIDs))
		for _, day := range edit.Days {
			id := strings.ToLower(strings.TrimSpace(day.ID))
			if _, ok := seen[id]; ok {
				return Schedule{}, 0, fmt.Errorf("schedule lists %s twice", id)
			}
			on, err := clockPtr(id+" on", day.On)
			if err != nil {
				return Schedule{}, 0, err
			}
			off, err := clockPtr(id+" off", day.Off)
			if err != nil {
				return Schedule{}, 0, err
			}
			seen[id] = Day{On: on, Off: off}
		}
		for i, id := range dayIDs {
			day, ok := seen[id]
			if !ok {
				return Schedule{}, 0, fmt.Errorf("schedule is missing %s", id)
			}
			if err := schedule.setDay(time.Weekday(i), day); err != nil {
				return Schedule{}, 0, err
			}
		}
	}
	if edit.EnsureOff != nil {
		ensure, err := clockPtr("ensure_off_daily", *edit.EnsureOff)
		if err != nil {
			return Schedule{}, 0, err
		}
		if ensure == nil {
			schedule.EnsureOffDaily = ""
		} else {
			schedule.EnsureOffDaily = *ensure
		}
	}
	if edit.DelayMinutes != nil {
		if *edit.DelayMinutes <= 0 {
			return Schedule{}, 0, errors.New("delay off step must be at least 1 minute")
		}
		minutes = *edit.DelayMinutes
	}
	return schedule, minutes, nil
}

func (s *Schedule) setDay(weekday time.Weekday, day Day) error {
	switch weekday {
	case time.Sunday:
		s.Sunday = day
	case time.Monday:
		s.Monday = day
	case time.Tuesday:
		s.Tuesday = day
	case time.Wednesday:
		s.Wednesday = day
	case time.Thursday:
		s.Thursday = day
	case time.Friday:
		s.Friday = day
	case time.Saturday:
		s.Saturday = day
	default:
		return fmt.Errorf("schedule day %s is unknown", weekday)
	}
	return nil
}

func clockPtr(name, value string) (*string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "none") || strings.EqualFold(value, "null") {
		return nil, nil
	}
	parsed := &value
	if err := checkClock(name, parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func clockText(value *string) string {
	if value == nil {
		return ""
	}
	text := strings.TrimSpace(*value)
	if text == "" || strings.EqualFold(text, "null") || strings.EqualFold(text, "none") {
		return ""
	}
	return text
}

func rewriteDocument(source []byte, next *File, writeSchedule, writeDelay bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		return nil, fmt.Errorf("parse av control config: %w", err)
	}
	root := documentMapping(&doc)
	if root == nil {
		return nil, errors.New("av control config: document is empty")
	}
	setKey(root, "switches", switchesNode(next.Switches))
	setKey(root, "startup", stepsNode(next.Startup))
	setDeveloper(root, next.Developer)
	if writeSchedule {
		setKey(root, "schedule", scheduleNode(next.Schedule))
	}
	if writeDelay {
		setOffDelay(root, next.OffDelay.StepMinutes)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode av control config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode av control config: %w", err)
	}
	return buf.Bytes(), nil
}

func documentMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0]
	}
	if doc.Kind == yaml.MappingNode {
		return doc
	}
	return nil
}

func setKey(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, scalar(key), value)
}

func setDeveloper(root *yaml.Node, dev Developer) {
	const key = "developer.tuya.com"
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value != key {
			continue
		}
		section := root.Content[i+1]
		if section.Kind != yaml.MappingNode {
			break
		}
		setScalar(section, "project_code", dev.ProjectCode)
		setScalar(section, "access_id", dev.AccessID)
		setScalar(section, "access_secret", dev.AccessSecret)
		return
	}
	section := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setScalar(section, "project_code", dev.ProjectCode)
	setScalar(section, "access_id", dev.AccessID)
	setScalar(section, "access_secret", dev.AccessSecret)
	if dev.Endpoint != "" {
		setScalar(section, "endpoint", dev.Endpoint)
	}
	setKey(root, key, section)
}

func setScalar(mapping *yaml.Node, key, value string) {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = value
			return
		}
	}
	mapping.Content = append(mapping.Content, scalar(key), scalar(value))
}

func switchesNode(switches []Switch) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, sw := range switches {
		item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		addScalar(item, "id", sw.ID)
		addScalar(item, "label", sw.Label)
		addScalar(item, "device_id", sw.DeviceID)
		if sw.IP != "" {
			addScalar(item, "ip", sw.IP)
		}
		addScalar(item, "local_key", sw.LocalKey)
		if sw.Protocol != "" {
			addScalar(item, "protocol", sw.Protocol)
		}
		if sw.Circuit != "" {
			addScalar(item, "circuit", sw.Circuit)
		}
		if sw.PowerRestore != "" {
			addScalar(item, "power_restore", sw.PowerRestore)
		}
		seq.Content = append(seq.Content, item)
	}
	return seq
}

func scheduleNode(schedule Schedule) *yaml.Node {
	section := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i, id := range dayIDs {
		day := schedule.Day(time.Weekday(i))
		item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		item.Content = append(item.Content,
			scalar("on"), clockNode(day.On),
			scalar("off"), clockNode(day.Off),
		)
		section.Content = append(section.Content, scalar(id), item)
	}
	section.Content = append(section.Content, scalar("ensure_off_daily"), clockNode(optionalClock(schedule.EnsureOffDaily)))
	return section
}

func setOffDelay(root *yaml.Node, minutes int) {
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value != "off_delay" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		section := root.Content[i+1]
		for j := 0; j < len(section.Content)-1; j += 2 {
			if section.Content[j].Value == "step_minutes" {
				section.Content[j+1].Tag = "!!int"
				section.Content[j+1].Value = strconv.Itoa(minutes)
				return
			}
		}
		section.Content = append(section.Content, scalar("step_minutes"), intScalar(minutes))
		return
	}
	section := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	section.Content = append(section.Content, scalar("step_minutes"), intScalar(minutes))
	setKey(root, "off_delay", section)
}

func clockNode(value *string) *yaml.Node {
	if text := clockText(value); text != "" {
		return scalar(text)
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}

func stepsNode(steps []Step) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, step := range steps {
		item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if step.WakeMacs && step.Switch == "" {
			item.Content = append(item.Content,
				scalar("wake_macs"), boolScalar(true),
				scalar("settle_sec"), intScalar(step.SettleSec),
			)
		} else {
			item.Content = append(item.Content,
				scalar("switch"), scalar(step.Switch),
				scalar("settle_sec"), intScalar(step.SettleSec),
			)
		}
		seq.Content = append(seq.Content, item)
	}
	return seq
}

func addScalar(mapping *yaml.Node, key, value string) {
	mapping.Content = append(mapping.Content, scalar(key), scalar(value))
}

func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func intScalar(value int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)}
}

func boolScalar(value bool) *yaml.Node {
	text := "false"
	if value {
		text = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: text}
}

func writeYAML(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
