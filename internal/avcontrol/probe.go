package avcontrol

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ProbeItem is one power-on entry after a connection check. The text is what
// the page shows; keys and the on/off state stay here.
type ProbeItem struct {
	Label   string `json:"label"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
}

// ProbeReport is the connection check for the list currently on the page.
type ProbeReport struct {
	OK      bool        `json:"ok"`
	Summary string      `json:"summary"`
	Items   []ProbeItem `json:"items"`
}

type switchProber interface {
	Probe(context.Context, *File, []Switch) map[string]Reach
}

type probeRow struct {
	id      string
	label   string
	summary string
}

// Probe checks each item in the form, in list order. A blank local key or
// access secret keeps the saved value. Nothing is written and no switch is
// turned on or off.
func (c *Controller) Probe(ctx context.Context, edit ConfigEdit) (ProbeReport, error) {
	c.reload()
	c.mu.Lock()
	file := c.file
	c.mu.Unlock()
	if file == nil {
		if c.problemText() != "" {
			return ProbeReport{}, errors.New(c.problemText())
		}
		return ProbeReport{}, ErrNotConfigured
	}

	next, rows, err := prepareProbe(file, edit)
	if err != nil {
		return ProbeReport{}, err
	}
	readings := map[string]Reach{}
	if len(next.Switches) > 0 {
		if prober, ok := c.commands.(switchProber); ok {
			readings = prober.Probe(ctx, next, next.Switches)
		} else {
			for id, reading := range c.commands.States(ctx, next.Switches) {
				via := ""
				if reading.Err == nil {
					via = "read"
				}
				readings[id] = Reach{On: reading.On, Via: via, Err: reading.Err}
			}
		}
	}

	report := ProbeReport{Items: make([]ProbeItem, len(rows)), OK: true}
	reached := 0
	for i, row := range rows {
		item := ProbeItem{Label: row.label, Summary: row.summary}
		if row.summary == "" {
			reach, ok := readings[row.id]
			if !ok || reach.Err != nil {
				msg := "no reply"
				if ok && reach.Err != nil {
					msg = shortErr(reach.Err)
				}
				item.Summary = "Not reachable. " + msg
			} else {
				item.OK = true
				reached++
				switch reach.Via {
				case "local":
					item.Summary = "Reachable on the local network"
				case "cloud":
					item.Summary = "Reachable through the Tuya cloud"
				default:
					item.Summary = "Reachable"
				}
			}
		}
		if !item.OK {
			report.OK = false
		}
		report.Items[i] = item
	}
	report.Summary = probeSummary(reached, len(rows))
	return report, nil
}

func prepareProbe(current *File, edit ConfigEdit) (*File, []probeRow, error) {
	if len(edit.Items) == 0 {
		return nil, nil, errors.New("add at least one item")
	}
	byID := make(map[string]Switch, len(current.Switches))
	for _, sw := range current.Switches {
		byID[sw.ID] = sw
	}

	secret, err := accessSecretFor(current.Developer, edit)
	if err != nil {
		return nil, nil, err
	}
	next := *current
	next.Developer.ProjectCode = strings.TrimSpace(edit.ProjectCode)
	next.Developer.AccessID = strings.TrimSpace(edit.AccessID)
	next.Developer.AccessSecret = secret

	used := make(map[string]bool, len(edit.Items))
	deviceOwner := make(map[string]string, len(edit.Items))
	rows := make([]probeRow, 0, len(edit.Items))
	switches := make([]Switch, 0, len(edit.Items))
	for i, item := range edit.Items {
		row := probeRow{label: strings.TrimSpace(item.Label)}
		if row.label == "" {
			row.label = fmt.Sprintf("Item %d", i+1)
			row.summary = "Needs a name"
			rows = append(rows, row)
			continue
		}
		deviceID := strings.TrimSpace(item.DeviceID)
		if deviceID == "" {
			row.summary = "Needs a device id"
			rows = append(rows, row)
			continue
		}
		if item.DelaySec < 0 {
			row.summary = "Delay must be zero or more"
			rows = append(rows, row)
			continue
		}
		ip, err := ipFor(item.IP)
		if err != nil {
			row.summary = "Needs a valid IP address"
			rows = append(rows, row)
			continue
		}
		if other := deviceOwner[deviceID]; other != "" {
			row.summary = "Uses the same device id as " + other
			rows = append(rows, row)
			continue
		}

		var sw Switch
		id := strings.TrimSpace(item.ID)
		if prev, ok := byID[id]; ok && id != "" {
			if used[id] {
				row.summary = "Listed more than once"
				rows = append(rows, row)
				continue
			}
			sw = prev
		}
		protocol, err := protocolFor(sw.Protocol, item.Protocol)
		if err != nil {
			row.summary = "Protocol must be 3.3, 3.4, or 3.5"
			rows = append(rows, row)
			continue
		}
		key, err := localKeyFor(sw, deviceID, ip, item.LocalKey)
		if err != nil {
			if errors.Is(err, errRetargetKey) {
				row.summary = "Enter the local key again for the new address"
			} else {
				row.summary = "Needs a local key"
			}
			rows = append(rows, row)
			continue
		}
		sw.Label = row.label
		sw.DeviceID = deviceID
		sw.IP = ip
		sw.Protocol = protocol
		sw.LocalKey = key
		if sw.ID == "" {
			sw.ID = uniqueID(row.label, used)
		}
		used[sw.ID] = true
		deviceOwner[deviceID] = row.label
		row.id = sw.ID
		switches = append(switches, sw)
		rows = append(rows, row)
	}
	next.Switches = switches
	return &next, rows, nil
}

func probeSummary(reached, total int) string {
	switch {
	case total == 1 && reached == 1:
		return "Reachable"
	case total == 1:
		return "Not reachable"
	case reached == total:
		return fmt.Sprintf("All %d items are reachable", total)
	case reached == 0:
		return fmt.Sprintf("None of the %d items are reachable", total)
	default:
		return fmt.Sprintf("%d of %d items are reachable", reached, total)
	}
}
