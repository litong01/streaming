package avcontrol

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strings"
	"time"
)

const (
	tuyaPrefix = 0x000055AA
	tuyaSuffix = 0x0000AA55
	cmdControl = 7
	cmdQuery   = 10
	tuyaPort   = "6668"
)

// localReady reports whether this switch has a real LAN key. Placeholder keys
// in the example file are skipped so the cloud path can run the test devices.
func localReady(sw Switch) bool {
	if net.ParseIP(sw.IP) == nil {
		return false
	}
	key := sw.LocalKey
	if len(key) != 16 {
		return false
	}
	if strings.Contains(strings.ToUpper(key), "REPLACE") {
		return false
	}
	return true
}

func protocolOf(sw Switch) string {
	switch strings.TrimSpace(sw.Protocol) {
	case "3.4":
		return "3.4"
	case "3.5":
		return "3.5"
	default:
		return "3.3"
	}
}

func localSet(ctx context.Context, sw Switch, dp string, on bool) error {
	if !localReady(sw) {
		return fmt.Errorf("no usable local key")
	}
	if dp == "" {
		dp = "1"
	}
	return localSession(ctx, sw, func(s *lanSession) error {
		return s.setSwitch(sw.DeviceID, dp, on)
	})
}

func localRead(ctx context.Context, sw Switch, dp string) (bool, error) {
	if !localReady(sw) {
		return false, fmt.Errorf("no usable local key")
	}
	if dp == "" {
		dp = "1"
	}
	var found bool
	var on bool
	err := localSession(ctx, sw, func(s *lanSession) error {
		reply, err := s.querySwitch(sw.DeviceID, dp)
		if err != nil {
			return err
		}
		parsed, err := parseDPS(reply)
		if err != nil {
			return err
		}
		if value, ok := parsed[dp]; ok {
			on, found = asBool(value)
			return nil
		}
		for _, key := range []string{"1", "20"} {
			if value, ok := parsed[key]; ok {
				on, found = asBool(value)
				return nil
			}
		}
		return fmt.Errorf("switch state missing")
	})
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("switch state missing")
	}
	return on, nil
}

func parseDPS(raw []byte) (map[string]any, error) {
	var body struct {
		DPS  map[string]any `json:"dps"`
		Data struct {
			DPS map[string]any `json:"dps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if len(body.DPS) > 0 {
		return body.DPS, nil
	}
	if len(body.Data.DPS) > 0 {
		return body.Data.DPS, nil
	}
	return nil, fmt.Errorf("no switch state")
}

func strconvNow() string {
	return fmt.Sprintf("%d", time.Now().Unix())
}

func encodeFrame(version string, cmd uint32, key string, payload any, versionHeader bool) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	enc, err := encryptECB([]byte(key), raw)
	if err != nil {
		return nil, err
	}
	body := enc
	if versionHeader {
		header := append([]byte(version), make([]byte, 12)...)
		body = append(header, enc...)
	}
	seq := uint32(time.Now().UnixNano() & 0x7fffffff)
	if seq == 0 {
		seq = 1
	}
	return packFrame(seq, cmd, []byte(key), body, version == "3.4"), nil
}

func packFrame(seq, cmd uint32, key, payload []byte, useHMAC bool) []byte {
	tail := 8
	if useHMAC {
		tail = 32 + 4
	}
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:], tuyaPrefix)
	binary.BigEndian.PutUint32(header[4:], seq)
	binary.BigEndian.PutUint32(header[8:], cmd)
	binary.BigEndian.PutUint32(header[12:], uint32(len(payload)+tail))
	frame := append(header, payload...)
	end := make([]byte, tail)
	if useHMAC {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(frame)
		copy(end[:32], mac.Sum(nil))
	} else {
		binary.BigEndian.PutUint32(end[0:], crc32.ChecksumIEEE(frame))
	}
	binary.BigEndian.PutUint32(end[len(end)-4:], tuyaSuffix)
	return append(frame, end...)
}

func decodePayload(version, key string, frame []byte) ([]byte, error) {
	if len(frame) < 20 {
		return nil, fmt.Errorf("short tuya frame")
	}
	payloadLen := int(binary.BigEndian.Uint32(frame[12:16]))
	if payloadLen < 8 || 16+payloadLen > len(frame) {
		return nil, fmt.Errorf("bad tuya frame")
	}
	tail := 8
	if version == "3.4" {
		tail = 36
	}
	payload := frame[16 : 16+payloadLen-tail]
	if bytes.HasPrefix(payload, []byte(version)) && len(payload) > 15 {
		payload = payload[15:]
	} else if len(payload) > 4 && payload[4] == '3' {
		payload = payload[4:]
		if len(payload) > 15 && payload[0] == '3' {
			payload = payload[15:]
		}
	}
	return decryptECB([]byte(key), payload)
}

func exchange(ctx context.Context, ip string, frame []byte, reply *[]byte) error {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, tuyaPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 4*time.Second {
		deadline = time.Now().Add(4 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(frame); err != nil {
		return err
	}
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 256)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if bytes.Contains(buf, []byte{0x00, 0x00, 0xAA, 0x55}) {
				break
			}
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				break
			}
			if len(buf) > 0 {
				break
			}
			return err
		}
		if len(buf) > 8192 {
			break
		}
	}
	if reply != nil {
		*reply = buf
	}
	return nil
}

func encryptECB(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain = pkcs7Pad(plain, aes.BlockSize)
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return out, nil
}

func decryptECB(key, cipherText []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(cipherText) == 0 || len(cipherText)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bad tuya payload")
	}
	out := make([]byte, len(cipherText))
	for i := 0; i < len(cipherText); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], cipherText[i:i+aes.BlockSize])
	}
	return pkcs7Unpad(out)
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty tuya payload")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > aes.BlockSize || n > len(b) {
		return nil, fmt.Errorf("bad tuya padding")
	}
	return b[:len(b)-n], nil
}
