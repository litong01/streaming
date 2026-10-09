package avcontrol

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	cmdDiscover    = 0x25
	discoverWait   = 5 * time.Second
	discoverTTL    = 30 * time.Minute
	tuyaPrefix6699 = 0x00006699
	tuyaSuffix6699 = 0x00009966
)

var errScanRunning = errors.New("a device scan is already running")

// FoundDevice is one linked device the configuration page can add. The local
// key stays on the server.
type FoundDevice struct {
	Name         string `json:"name"`
	DeviceID     string `json:"deviceId"`
	IP           string `json:"ip,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	Product      string `json:"product,omitempty"`
	HasLocalKey  bool   `json:"hasLocalKey"`
	OnNetwork    bool   `json:"onNetwork"`
	AlreadyAdded bool   `json:"alreadyAdded"`
}

// DiscoverReport is the import list for the credentials currently on the page.
type DiscoverReport struct {
	Summary string        `json:"summary"`
	Devices []FoundDevice `json:"devices"`
}

type rememberedDevice struct {
	key      string
	ip       string
	protocol string
}

type catalogDevice struct {
	name      string
	id        string
	key       string
	product   string
	ip        string
	protocol  string
	candidate string
	sub       bool
	onNetwork bool
}

type lanDevice struct {
	ID       string
	IP       string
	Protocol string
}

type ifaceAddr struct {
	ip        net.IP
	mask      net.IPMask
	broadcast string
}

// Discover asks Tuya for every device linked to the project, then listens on
// this server's network for each device's address and protocol. Nothing is
// written until Save.
func (c *Controller) Discover(ctx context.Context, edit ConfigEdit) (DiscoverReport, error) {
	c.reload()
	c.mu.Lock()
	file := c.file
	c.mu.Unlock()
	if file == nil {
		if c.problemText() != "" {
			return DiscoverReport{}, errors.New(c.problemText())
		}
		return DiscoverReport{}, ErrNotConfigured
	}
	if strings.TrimSpace(edit.ProjectCode) == "" || strings.TrimSpace(edit.AccessID) == "" {
		return DiscoverReport{}, errors.New("project code and access id are required")
	}
	secret, err := accessSecretFor(file.Developer, edit)
	if err != nil {
		return DiscoverReport{}, err
	}
	if strings.TrimSpace(secret) == "" {
		return DiscoverReport{}, errors.New("access secret is required")
	}
	if !c.scanMu.TryLock() {
		return DiscoverReport{}, errScanRunning
	}
	defer c.scanMu.Unlock()

	sweep := shouldSweep(ctx)
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()

	client := newCloudClient()
	client.configure(Developer{
		ProjectCode:  strings.TrimSpace(edit.ProjectCode),
		AccessID:     strings.TrimSpace(edit.AccessID),
		AccessSecret: secret,
		Endpoint:     file.Developer.Endpoint,
	})

	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()
	lanCh := make(chan []lanDevice, 1)
	go func() {
		lanCh <- scanLAN(scanCtx, discoverWait)
	}()
	listed, err := client.devices(ctx)
	if err != nil {
		scanCancel()
		<-lanCh
		return DiscoverReport{}, err
	}
	lan := <-lanCh
	merged := mergeCatalog(listed, lan)
	if sweep {
		merged = fillFromNetwork(ctx, merged)
	}
	c.remember(merged)
	report := reportFrom(file, merged)
	log.Printf("av control: listed %d linked devices, %d with a LAN address", len(report.Devices), countOnNetwork(report.Devices))
	return report, nil
}

func shouldSweep(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > 2*time.Second
}

func (c *Controller) remember(devices []catalogDevice) {
	c.foundMu.Lock()
	defer c.foundMu.Unlock()
	c.found = map[string]rememberedDevice{}
	c.foundAt = time.Now()
	for _, device := range devices {
		if device.id == "" || device.key == "" {
			continue
		}
		c.found[device.id] = rememberedDevice{key: device.key, ip: device.ip, protocol: device.protocol}
	}
}

// withDiscoveredKeys fills a blank local key for a device that is not already
// saved. A key already in the file is left alone.
func (c *Controller) withDiscoveredKeys(file *File, edit ConfigEdit) ConfigEdit {
	c.foundMu.Lock()
	defer c.foundMu.Unlock()
	if len(c.found) == 0 || time.Since(c.foundAt) > discoverTTL {
		return edit
	}
	saved := make(map[string]Switch, len(file.Switches))
	for _, sw := range file.Switches {
		saved[sw.ID] = sw
	}
	edit.Items = append([]ItemEdit(nil), edit.Items...)
	for i := range edit.Items {
		item := &edit.Items[i]
		if strings.TrimSpace(item.LocalKey) != "" {
			continue
		}
		deviceID := strings.TrimSpace(item.DeviceID)
		prev, exists := saved[strings.TrimSpace(item.ID)]
		if exists && storedSecret(prev.LocalKey) != "" && strings.TrimSpace(prev.DeviceID) == deviceID {
			continue
		}
		remembered, ok := c.found[deviceID]
		if !ok || remembered.key == "" {
			continue
		}
		ip := strings.TrimSpace(item.IP)
		if remembered.ip != "" && ip != "" && remembered.ip != ip {
			continue
		}
		item.LocalKey = remembered.key
		if strings.TrimSpace(item.Protocol) == "" && remembered.protocol != "" {
			item.Protocol = remembered.protocol
		}
	}
	return edit
}

func reportFrom(file *File, devices []catalogDevice) DiscoverReport {
	added := map[string]bool{}
	for _, sw := range file.Switches {
		if id := strings.TrimSpace(sw.DeviceID); id != "" {
			added[id] = true
		}
	}
	out := make([]FoundDevice, 0, len(devices))
	for _, device := range devices {
		out = append(out, FoundDevice{
			Name:         device.name,
			DeviceID:     device.id,
			IP:           device.ip,
			Protocol:     device.protocol,
			Product:      device.product,
			HasLocalKey:  device.key != "",
			OnNetwork:    device.onNetwork && device.ip != "",
			AlreadyAdded: added[device.id],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].Name)
		right := strings.ToLower(out[j].Name)
		if left == right {
			return out[i].DeviceID < out[j].DeviceID
		}
		return left < right
	})
	return DiscoverReport{Summary: discoverSummary(out), Devices: out}
}

func discoverSummary(devices []FoundDevice) string {
	if len(devices) == 0 {
		return "No devices are linked to this Tuya project."
	}
	onNet := countOnNetwork(devices)
	switch {
	case len(devices) == 1 && onNet == 1:
		return "Found 1 device, with a LAN address."
	case len(devices) == 1:
		return "Found 1 device. No LAN address was found."
	case onNet == len(devices):
		return fmt.Sprintf("Found %d devices, with a LAN address for each.", len(devices))
	default:
		return fmt.Sprintf("Found %d devices. %d have a LAN address.", len(devices), onNet)
	}
}

func countOnNetwork(devices []FoundDevice) int {
	n := 0
	for _, device := range devices {
		if device.OnNetwork {
			n++
		}
	}
	return n
}

func mergeCatalog(listed []cloudDevice, lan []lanDevice) []catalogDevice {
	byID := make(map[string]lanDevice, len(lan))
	for _, hit := range lan {
		if hit.ID == "" || net.ParseIP(hit.IP) == nil {
			continue
		}
		byID[hit.ID] = hit
	}
	var out []catalogDevice
	seen := map[string]bool{}
	for _, device := range listed {
		id := device.identity()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		item := catalogDevice{
			name:      strings.TrimSpace(device.Name),
			id:        id,
			key:       strings.TrimSpace(device.LocalKey),
			product:   strings.TrimSpace(device.Product),
			candidate: lanIP(device.IP),
			sub:       device.Sub,
		}
		if item.name == "" {
			item.name = item.product
		}
		if item.name == "" {
			item.name = id
		}
		if hit, ok := byID[id]; ok {
			if ip := lanIP(hit.IP); ip != "" {
				item.ip = ip
				item.protocol = hit.Protocol
				item.onNetwork = true
			}
		}
		out = append(out, item)
	}
	return out
}

func lanIP(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
		return ""
	}
	return ip.To4().String()
}

func fillFromNetwork(ctx context.Context, devices []catalogDevice) []catalogDevice {
	var pending []int
	for i := range devices {
		if devices[i].onNetwork || devices[i].sub || devices[i].key == "" {
			continue
		}
		pending = append(pending, i)
	}
	if len(pending) == 0 || ctx.Err() != nil {
		return devices
	}
	claimed := map[string]bool{}
	for _, device := range devices {
		if device.onNetwork && device.ip != "" {
			claimed[device.ip] = true
		}
	}
	var still []int
	for _, index := range pending {
		candidate := devices[index].candidate
		if candidate == "" || claimed[candidate] {
			still = append(still, index)
			continue
		}
		if version, ok := identifyLocal(ctx, candidate, devices[index]); ok {
			devices[index].ip = candidate
			devices[index].protocol = version
			devices[index].onNetwork = true
			claimed[candidate] = true
			rememberProtocol(candidate, version)
			continue
		}
		still = append(still, index)
	}
	if len(still) == 0 || ctx.Err() != nil {
		return devices
	}
	// Every open port is tried until the scan deadline. A fixed prefix of the
	// sorted list previously hid the Ethernet LAN behind another subnet.
	open := probePort(ctx, subnetHosts(claimed))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, ip := range open {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			indexes := append([]int(nil), still...)
			mu.Unlock()
			for _, index := range indexes {
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				if devices[index].onNetwork || claimed[ip] {
					mu.Unlock()
					continue
				}
				mu.Unlock()
				version, ok := identifyLocal(ctx, ip, devices[index])
				if !ok {
					continue
				}
				mu.Lock()
				free := !devices[index].onNetwork && !claimed[ip]
				if free {
					devices[index].ip = ip
					devices[index].protocol = version
					devices[index].onNetwork = true
					claimed[ip] = true
					rememberProtocol(ip, version)
				}
				ipTaken := claimed[ip]
				mu.Unlock()
				if free || ipTaken {
					return
				}
			}
		}(ip)
	}
	wg.Wait()
	return devices
}

func identifyLocal(ctx context.Context, ip string, device catalogDevice) (string, bool) {
	if !localReady(Switch{IP: ip, LocalKey: device.key}) {
		return "", false
	}
	for _, version := range []string{"3.5", "3.4", "3.3"} {
		if ctx.Err() != nil {
			return "", false
		}
		attempt, cancel := context.WithTimeout(ctx, 3*time.Second)
		sw := Switch{DeviceID: device.id, IP: ip, LocalKey: device.key, Protocol: version}
		err := withSession(attempt, sw, version, func(s *lanSession) error {
			reply, err := s.queryDPS(device.id, "1")
			if err != nil {
				return err
			}
			if !replyMatchesDevice(reply, device.id) {
				return fmt.Errorf("unrecognized")
			}
			return nil
		})
		cancel()
		if err == nil {
			return version, true
		}
	}
	return "", false
}

func rememberProtocol(ip, version string) {
	if ip != "" && version != "" {
		learnedProtocol.Store(ip, version)
	}
}

func replyMatchesDevice(reply []byte, id string) bool {
	if rejected(reply) {
		return false
	}
	var body struct {
		GwID  string         `json:"gwId"`
		DevID string         `json:"devId"`
		DPS   map[string]any `json:"dps"`
	}
	if err := json.Unmarshal(jsonObject(reply), &body); err != nil {
		return false
	}
	if body.DevID != "" && body.DevID != id {
		return false
	}
	if body.GwID != "" && body.GwID != id {
		return false
	}
	if body.DPS != nil {
		return true
	}
	return body.DevID == id || body.GwID == id
}

func probePort(ctx context.Context, hosts []string) []string {
	var mu sync.Mutex
	var open []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dialer := net.Dialer{Timeout: time.Second}
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, tuyaPort))
			if err != nil {
				return
			}
			_ = conn.Close()
			mu.Lock()
			open = append(open, host)
			mu.Unlock()
		}(host)
	}
	wg.Wait()
	sort.Strings(open)
	return open
}

func subnetHosts(skip map[string]bool) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, iface := range localIPv4() {
		ones, bits := iface.mask.Size()
		if bits != 32 || ones >= 31 {
			continue
		}
		mask := iface.mask
		if ones < 24 {
			mask = net.CIDRMask(24, 32)
			ones = 24
		}
		network := iface.ip.Mask(mask).To4()
		base := binary.BigEndian.Uint32(network)
		count := 1 << (32 - ones)
		for n := 1; n < count-1; n++ {
			ip := make(net.IP, 4)
			binary.BigEndian.PutUint32(ip, base+uint32(n))
			text := ip.String()
			if text == iface.ip.String() || skip[text] || seen[text] {
				continue
			}
			seen[text] = true
			hosts = append(hosts, text)
		}
	}
	return hosts
}

func scanLAN(ctx context.Context, wait time.Duration) []lanDevice {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	var conns []*net.UDPConn
	for _, port := range []int{6666, 6667, 7000} {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
		if err != nil {
			continue
		}
		enableBroadcast(conn)
		conns = append(conns, conn)
	}
	if len(conns) == 0 {
		return nil
	}
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()

	var sender *net.UDPConn
	for _, conn := range conns {
		if conn.LocalAddr().(*net.UDPAddr).Port == 7000 {
			sender = conn
			break
		}
	}
	if sender == nil {
		sender = conns[0]
	}
	go func() {
		solicit(sender)
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			solicit(sender)
		}
	}()

	found := map[string]lanDevice{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func(conn *net.UDPConn) {
			defer wg.Done()
			buf := make([]byte, 2048)
			for ctx.Err() == nil {
				deadline := time.Now().Add(200 * time.Millisecond)
				if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
					deadline = ctxDeadline
				}
				_ = conn.SetReadDeadline(deadline)
				n, _, err := conn.ReadFromUDP(buf)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					var netErr net.Error
					if errors.As(err, &netErr) && netErr.Timeout() {
						continue
					}
					return
				}
				dev, ok := decodeDiscoveryPacket(append([]byte(nil), buf[:n]...))
				if !ok {
					continue
				}
				mu.Lock()
				prev, exists := found[dev.ID]
				if !exists || prev.IP == "" || (prev.Protocol == "" && dev.Protocol != "") {
					if exists {
						if dev.IP == "" {
							dev.IP = prev.IP
						}
						if dev.Protocol == "" {
							dev.Protocol = prev.Protocol
						}
					}
					found[dev.ID] = dev
				}
				mu.Unlock()
			}
		}(conn)
	}
	<-ctx.Done()
	for _, conn := range conns {
		_ = conn.Close()
	}
	wg.Wait()

	out := make([]lanDevice, 0, len(found))
	for _, dev := range found {
		out = append(out, dev)
	}
	return out
}

func solicit(conn *net.UDPConn) {
	key := udpKey()
	for _, iface := range localIPv4() {
		payload := []byte(`{"from":"app","ip":"` + iface.ip.String() + `"}`)
		packet, err := pack6699(1, cmdDiscover, key, payload, nil)
		if err != nil {
			continue
		}
		for _, target := range []string{iface.broadcast, "255.255.255.255"} {
			addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(target, "7000"))
			if err != nil {
				continue
			}
			_, _ = conn.WriteToUDP(packet, addr)
		}
	}
}

func enableBroadcast(conn *net.UDPConn) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})
}

func usableLAN(iface net.Interface) bool {
	flags := iface.Flags
	if flags&net.FlagUp == 0 || flags&net.FlagLoopback != 0 {
		return false
	}
	// A point-to-point tunnel answers for addresses that are not plugs.
	// Those replies crowd out the Ethernet LAN the switches are on.
	return flags&net.FlagPointToPoint == 0
}

func localIPv4() []ifaceAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []ifaceAddr
	for _, iface := range ifaces {
		if !usableLAN(iface) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if lanIP(ip.String()) == "" {
				continue
			}
			mask := ipnet.Mask
			if len(mask) != net.IPv4len {
				continue
			}
			out = append(out, ifaceAddr{ip: ip, mask: mask, broadcast: broadcastAddr(ip, mask)})
		}
	}
	return out
}

func broadcastAddr(ip net.IP, mask net.IPMask) string {
	out := make(net.IP, net.IPv4len)
	for i := 0; i < net.IPv4len; i++ {
		out[i] = ip[i] | ^mask[i]
	}
	return out.String()
}

func udpKey() []byte {
	sum := md5.Sum([]byte("yGAdlopoPVldABfn"))
	return sum[:]
}

func decodeDiscoveryPacket(packet []byte) (lanDevice, bool) {
	for _, text := range discoveryTexts(packet) {
		if dev, ok := parseDiscoveryJSON(text); ok {
			return dev, true
		}
	}
	return lanDevice{}, false
}

func discoveryTexts(packet []byte) []string {
	var texts []string
	add := func(raw []byte) {
		if text := jsonObject(raw); len(text) > 0 {
			texts = append(texts, string(text))
		}
	}
	add(packet)
	if bytes.HasPrefix(packet, []byte{0x00, 0x00, 0x55, 0xAA}) {
		for _, payload := range framedPayloads(packet) {
			add(payload)
			if plain, err := decryptECB(udpKey(), payload); err == nil {
				add(plain)
			}
		}
	}
	if bytes.HasPrefix(packet, []byte{0x00, 0x00, 0x66, 0x99}) {
		if plain, err := decrypt6699(packet, udpKey()); err == nil {
			add(plain)
		}
	}
	if plain, err := decryptECB(udpKey(), packet); err == nil {
		add(plain)
	}
	return texts
}

func framedPayloads(packet []byte) [][]byte {
	if len(packet) < 20 || binary.BigEndian.Uint32(packet[0:4]) != tuyaPrefix {
		return nil
	}
	n := int(binary.BigEndian.Uint32(packet[12:16]))
	if n < 8 || 16+n > len(packet) {
		return nil
	}
	body := packet[16 : 16+n]
	var out [][]byte
	for _, tail := range []int{8, 36} {
		if len(body) <= tail {
			continue
		}
		payload := body[:len(body)-tail]
		out = append(out, payload)
		if len(payload) > 4 {
			out = append(out, payload[4:])
		}
	}
	return out
}

func parseDiscoveryJSON(text string) (lanDevice, bool) {
	var body struct {
		IP      string `json:"ip"`
		GwID    string `json:"gwId"`
		DevID   string `json:"devId"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		return lanDevice{}, false
	}
	id := strings.TrimSpace(body.GwID)
	if id == "" {
		id = strings.TrimSpace(body.DevID)
	}
	ip := net.ParseIP(strings.TrimSpace(body.IP))
	if id == "" || ip == nil || ip.To4() == nil {
		return lanDevice{}, false
	}
	return lanDevice{ID: id, IP: ip.To4().String(), Protocol: protocolVersion(body.Version)}, true
}

func protocolVersion(value string) string {
	switch strings.TrimSpace(value) {
	case "3.3", "3.4", "3.5":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func pack6699(seq, cmd uint32, key, plain, iv []byte) ([]byte, error) {
	if len(iv) == 0 {
		iv = make([]byte, 12)
		if _, err := rand.Read(iv); err != nil {
			return nil, err
		}
	}
	if len(iv) != 12 {
		return nil, fmt.Errorf("bad discovery iv")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	length := 12 + len(plain) + gcm.Overhead()
	header := make([]byte, 18)
	binary.BigEndian.PutUint32(header[0:], tuyaPrefix6699)
	binary.BigEndian.PutUint32(header[6:], seq)
	binary.BigEndian.PutUint32(header[10:], cmd)
	binary.BigEndian.PutUint32(header[14:], uint32(length))
	sealed := gcm.Seal(nil, iv, plain, header[4:])
	out := make([]byte, 0, len(header)+len(iv)+len(sealed)+4)
	out = append(out, header...)
	out = append(out, iv...)
	out = append(out, sealed...)
	out = append(out, 0x00, 0x00, 0x99, 0x66)
	return out, nil
}

func decrypt6699(packet, key []byte) ([]byte, error) {
	if len(packet) < 18+12+16+4 || binary.BigEndian.Uint32(packet[0:4]) != tuyaPrefix6699 {
		return nil, fmt.Errorf("short discovery packet")
	}
	length := int(binary.BigEndian.Uint32(packet[14:18]))
	if length < 12+16 || 18+length+4 > len(packet) {
		return nil, fmt.Errorf("bad discovery packet")
	}
	if binary.BigEndian.Uint32(packet[18+length:18+length+4]) != tuyaSuffix6699 {
		return nil, fmt.Errorf("bad discovery packet")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv := packet[18:30]
	body := packet[30 : 18+length]
	return gcm.Open(nil, iv, body, packet[4:18])
}
