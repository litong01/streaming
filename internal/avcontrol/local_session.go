package avcontrol

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	cmdKeyStart   = 3
	cmdKeyReply   = 4
	cmdKeyFinish  = 5
	cmdControlNew = 13
	cmdQueryNew   = 16
)

// learnedProtocol remembers which local protocol answered, so the next press
// does not repeat a handshake the switch already rejected.
var learnedProtocol sync.Map

type lanSession struct {
	conn    net.Conn
	seq     uint32
	key     []byte
	realKey []byte
	version string
}

func localSession(ctx context.Context, sw Switch, fn func(*lanSession) error) error {
	var tries []string
	for _, version := range protocolsToTry(sw) {
		err := withSession(ctx, sw, version, fn)
		if err == nil {
			learnedProtocol.Store(sw.IP, version)
			return nil
		}
		tries = append(tries, version+": "+err.Error())
	}
	if len(tries) == 0 {
		return fmt.Errorf("local control failed")
	}
	return fmt.Errorf("%s", strings.Join(tries, "; "))
}

func protocolsToTry(sw Switch) []string {
	if learned, ok := learnedProtocol.Load(sw.IP); ok {
		return []string{learned.(string)}
	}
	first := protocolOf(sw)
	out := []string{first}
	for _, version := range []string{"3.4", "3.3"} {
		if version != first && version != "3.5" {
			out = append(out, version)
		}
	}
	return out
}

func withSession(ctx context.Context, sw Switch, version string, fn func(*lanSession) error) error {
	if version == "3.5" {
		return fmt.Errorf("local protocol 3.5 is unsupported")
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(sw.IP, tuyaPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	session := &lanSession{
		conn:    conn,
		seq:     1,
		key:     []byte(sw.LocalKey),
		realKey: []byte(sw.LocalKey),
		version: version,
	}
	if version == "3.4" {
		if err := session.negotiate34(); err != nil {
			return err
		}
	}
	return fn(session)
}

func (s *lanSession) negotiate34() error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	if err := s.writeEncrypted(cmdKeyStart, nonce, s.realKey, false); err != nil {
		return err
	}
	reply, err := s.readEncrypted(s.realKey)
	if err != nil {
		return fmt.Errorf("session handshake: %w", err)
	}
	if reply.cmd != cmdKeyReply || len(reply.plain) < 48 {
		return fmt.Errorf("session handshake rejected")
	}
	remote := reply.plain[:16]
	check := hmacSHA256(s.realKey, nonce)
	if !hmac.Equal(check, reply.plain[16:48]) {
		return fmt.Errorf("session handshake mismatch")
	}
	finish := hmacSHA256(s.realKey, remote)
	if err := s.writeEncrypted(cmdKeyFinish, finish, s.realKey, false); err != nil {
		return err
	}
	mixed := make([]byte, 16)
	for i := range mixed {
		mixed[i] = nonce[i] ^ remote[i]
	}
	sessionKey, err := encryptECBNoPad(s.realKey, mixed)
	if err != nil {
		return err
	}
	s.key = sessionKey
	return nil
}

func (s *lanSession) setSwitch(id, dp string, on bool) error {
	if dp == "" {
		dp = "1"
	}
	if s.version == "3.4" {
		body := control34{
			Protocol: 5,
			T:        time.Now().Unix(),
		}
		body.Data.DPS = map[string]any{dp: on}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return s.command(cmdControlNew, raw, true)
	}
	raw, err := json.Marshal(map[string]any{
		"devId": id,
		"uid":   id,
		"t":     strconvNow(),
		"dps":   map[string]any{dp: on},
	})
	if err != nil {
		return err
	}
	_, err = s.request33(cmdControl, raw)
	return err
}

func (s *lanSession) querySwitch(id, dp string) ([]byte, error) {
	if dp == "" {
		dp = "1"
	}
	if s.version == "3.4" {
		reply, err := s.request(cmdQueryNew, []byte("{}"), false)
		if err == nil && !rejected(reply) {
			if _, perr := parseDPS(reply); perr == nil {
				return reply, nil
			}
		}
		// Some 22-character ids want the older query shape on command 13.
		raw, err := json.Marshal(map[string]any{
			"devId": id,
			"uid":   id,
			"t":     strconvNow(),
			"dps":   map[string]any{dp: nil},
		})
		if err != nil {
			return nil, err
		}
		return s.request(cmdControlNew, raw, true)
	}
	raw, err := json.Marshal(map[string]any{
		"gwId":  id,
		"devId": id,
		"uid":   id,
		"t":     strconvNow(),
	})
	if err != nil {
		return nil, err
	}
	return s.request33(cmdQuery, raw)
}

type control34 struct {
	Protocol int   `json:"protocol"`
	T        int64 `json:"t"`
	Data     struct {
		DPS map[string]any `json:"dps"`
	} `json:"data"`
}

func (s *lanSession) command(cmd uint32, plain []byte, header bool) error {
	reply, err := s.request(cmd, plain, header)
	if err != nil {
		return err
	}
	if rejected(reply) {
		return fmt.Errorf("device rejected command")
	}
	return nil
}

func rejected(reply []byte) bool {
	return bytes.Contains(reply, []byte("unvalid")) || bytes.Contains(reply, []byte("json obj data"))
}

func (s *lanSession) request(cmd uint32, plain []byte, header bool) ([]byte, error) {
	if s.version == "3.3" {
		return s.request33(cmd, plain)
	}
	if err := s.writeEncrypted(cmd, plain, s.key, header); err != nil {
		return nil, err
	}
	reply, err := s.readEncrypted(s.key)
	if err != nil {
		return nil, err
	}
	return jsonObject(reply.plain), nil
}

func (s *lanSession) request33(cmd uint32, plain []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, err
	}
	frame, err := encodeFrame("3.3", cmd, string(s.realKey), payload, cmd == cmdControl)
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(frame); err != nil {
		return nil, err
	}
	raw, err := readFrame(s.conn, false)
	if err != nil {
		return nil, err
	}
	decoded, err := decryptECB(s.realKey, raw.payload)
	if err != nil {
		return nil, err
	}
	return jsonObject(stripVersion(decoded)), nil
}

func (s *lanSession) writeEncrypted(cmd uint32, plain, key []byte, versionHeader bool) error {
	body := plain
	if versionHeader {
		body = append(append([]byte(s.version), make([]byte, 12)...), plain...)
	}
	enc, err := encryptECB(key, body)
	if err != nil {
		return err
	}
	frame := packFrame(s.seq, cmd, key, enc, true)
	s.seq++
	_, err = s.conn.Write(frame)
	return err
}

type lanReply struct {
	cmd   uint32
	plain []byte
}

func (s *lanSession) readEncrypted(key []byte) (lanReply, error) {
	raw, err := readFrame(s.conn, true)
	if err != nil {
		return lanReply{}, err
	}
	payload := raw.payload
	if len(payload) == 0 {
		return lanReply{cmd: raw.cmd}, nil
	}
	// Some replies put the version text in the clear, ahead of the ciphertext.
	if len(payload) >= 15 && payload[0] == '3' && payload[1] == '.' && (len(payload)-15)%aes.BlockSize == 0 {
		payload = payload[15:]
	}
	if len(payload)%aes.BlockSize != 0 {
		return lanReply{}, fmt.Errorf("local reply length %d", len(raw.payload))
	}
	plain, err := decryptECB(key, payload)
	if err != nil {
		return lanReply{}, err
	}
	return lanReply{cmd: raw.cmd, plain: stripVersion(plain)}, nil
}

type rawFrame struct {
	cmd     uint32
	payload []byte
}

func readFrame(conn net.Conn, hmacFrame bool) (rawFrame, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return rawFrame{}, err
	}
	if binary.BigEndian.Uint32(header[0:4]) != tuyaPrefix {
		return rawFrame{}, fmt.Errorf("unexpected local reply")
	}
	length := int(binary.BigEndian.Uint32(header[12:16]))
	if length < 8 || length > 8192 {
		return rawFrame{}, fmt.Errorf("unexpected local reply")
	}
	rest := make([]byte, length)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return rawFrame{}, err
	}
	tail := 8
	if hmacFrame {
		tail = 36
	}
	if length < tail+4 || binary.BigEndian.Uint32(rest[length-4:]) != tuyaSuffix {
		return rawFrame{}, fmt.Errorf("unexpected local reply")
	}
	// Replies start with a 4-byte return code. The integrity field and footer
	// sit at the end.
	return rawFrame{
		cmd:     binary.BigEndian.Uint32(header[8:12]),
		payload: append([]byte(nil), rest[4:length-tail]...),
	}, nil
}

func stripVersion(plain []byte) []byte {
	if len(plain) >= 15 && plain[0] == '3' && plain[1] == '.' {
		return plain[15:]
	}
	return plain
}

func jsonObject(plain []byte) []byte {
	start := bytes.IndexByte(plain, '{')
	end := bytes.LastIndexByte(plain, '}')
	if start >= 0 && end > start {
		return plain[start : end+1]
	}
	return plain
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func encryptECBNoPad(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(plain) == 0 || len(plain)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("bad session key")
	}
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += block.BlockSize() {
		block.Encrypt(out[i:i+block.BlockSize()], plain[i:i+block.BlockSize()])
	}
	return out, nil
}
