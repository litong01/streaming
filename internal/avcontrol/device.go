package avcontrol

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Reading is one switch, as that switch reported it. Err is set when the
// switch could not be read; On is then meaningless.
type Reading struct {
	On  bool
	Err error
}

// Commander talks to the switches. Tests substitute a fake; the server uses
// the LAN client and falls back to the Tuya cloud.
type Commander interface {
	Set(ctx context.Context, sw Switch, on bool) error
	States(ctx context.Context, switches []Switch) map[string]Reading
	Use(*File)
}

type deviceClient struct {
	cloud *cloudClient
	mu    sync.Mutex
	file  *File
	dps   map[string]string
}

func newDeviceClient() *deviceClient {
	return &deviceClient{
		cloud: newCloudClient(),
		dps:   map[string]string{},
	}
}

func (d *deviceClient) Use(file *File) {
	d.mu.Lock()
	d.file = file
	d.mu.Unlock()
	if file != nil {
		d.cloud.configure(file.Developer)
	}
}

func (d *deviceClient) preferLocal() (bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return false, true
	}
	return d.file.Control.PreferLocal, d.file.Control.CloudFallback
}

func (d *deviceClient) Set(ctx context.Context, sw Switch, on bool) error {
	preferLocal, cloudFallback := d.preferLocal()
	if preferLocal && localReady(sw) {
		err := localSet(ctx, sw, d.dp(ctx, sw), on)
		if err == nil {
			log.Printf("av control: %s %s via local", sw.ID, onOff(on))
			return nil
		}
		if !cloudFallback {
			return err
		}
		log.Printf("av control: %s local failed (%v); trying cloud", sw.ID, err)
	} else if preferLocal && !cloudFallback {
		return fmt.Errorf("%s has no usable local key", sw.ID)
	}
	if err := d.cloud.Set(ctx, sw.DeviceID, on); err != nil {
		return err
	}
	log.Printf("av control: %s %s via cloud", sw.ID, onOff(on))
	return nil
}

func (d *deviceClient) States(ctx context.Context, switches []Switch) map[string]Reading {
	out := make(map[string]Reading, len(switches))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, sw := range switches {
		wg.Add(1)
		go func(sw Switch) {
			defer wg.Done()
			on, err := d.readOne(ctx, sw)
			mu.Lock()
			out[sw.ID] = Reading{On: on, Err: err}
			mu.Unlock()
		}(sw)
	}
	wg.Wait()
	return out
}

func (d *deviceClient) readOne(ctx context.Context, sw Switch) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	preferLocal, cloudFallback := d.preferLocal()
	dp := d.cachedDP(sw.ID)
	var localErr error
	if preferLocal && localReady(sw) {
		on, err := localRead(ctx, sw, dp)
		if err == nil {
			return on, nil
		}
		localErr = err
		if !cloudFallback {
			return false, err
		}
	} else if preferLocal && !cloudFallback {
		return false, fmt.Errorf("no usable local key")
	}
	on, err := d.cloud.readSwitch(ctx, sw.DeviceID)
	if err != nil && localErr != nil {
		return false, fmt.Errorf("local: %s; cloud: %s", shortErr(localErr), shortErr(err))
	}
	return on, err
}

func (d *deviceClient) dp(ctx context.Context, sw Switch) string {
	if dp := d.cachedDP(sw.ID); dp != "" {
		return dp
	}
	code := ""
	if d.cloud.ready() {
		if discovered, err := d.cloud.switchCode(ctx, sw.DeviceID); err == nil {
			code = discovered
		}
	}
	dp := dpForCode(code)
	d.mu.Lock()
	d.dps[sw.ID] = dp
	d.mu.Unlock()
	return dp
}

func (d *deviceClient) cachedDP(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dps[id]
}

func dpForCode(code string) string {
	switch code {
	case "switch_led", "led_switch":
		return "20"
	case "switch_2":
		return "2"
	case "switch_3":
		return "3"
	case "switch_4":
		return "4"
	default:
		return "1"
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
