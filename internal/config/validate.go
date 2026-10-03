package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
)

const (
	minUserPort = 1024
	maxPort     = 65535
	maxHostLen  = 253
	maxUserLen  = 64
)

func ParseHostPort(address string, explicitPort int) (string, int, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", 0, fmt.Errorf("SMP host is required")
	}
	if strings.ContainsAny(address, " \t\r\n") {
		return "", 0, fmt.Errorf("SMP host must not contain spaces")
	}
	if strings.Contains(strings.ToLower(address), "://") {
		return "", 0, fmt.Errorf("SMP host must be a hostname or IP, not a URL")
	}

	host := address
	port := explicitPort
	if port == 0 {
		port = DefaultSmpPort
	}

	if strings.HasPrefix(address, "[") {
		if h, p, err := net.SplitHostPort(address); err == nil {
			host = h
			parsed, err := parsePort(p)
			if err != nil {
				return "", 0, err
			}
			port = parsed
		} else if strings.HasSuffix(address, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
		} else {
			return "", 0, fmt.Errorf("invalid SMP address")
		}
	} else if h, p, err := net.SplitHostPort(address); err == nil {
		host = h
		parsed, err := parsePort(p)
		if err != nil {
			return "", 0, err
		}
		port = parsed
	}

	host = strings.TrimSpace(host)
	if host == "" || len(host) > maxHostLen || !validHost(host) {
		return "", 0, fmt.Errorf("invalid SMP host")
	}
	if port < 1 || port > maxPort {
		return "", 0, fmt.Errorf("SMP port must be between 1 and %d", maxPort)
	}
	return host, port, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.SmpHost) == "" || strings.TrimSpace(c.SmpUsername) == "" {
		return fmt.Errorf("SMP host and username are required")
	}
	if _, _, err := ParseHostPort(c.SmpHost, c.SmpPort); err != nil {
		return err
	}
	username := strings.TrimSpace(c.SmpUsername)
	if len(username) > maxUserLen || containsControl(username) {
		return fmt.Errorf("invalid SMP username")
	}
	if c.SmpPort < 1 || c.SmpPort > maxPort {
		return fmt.Errorf("SMP port must be between 1 and %d", maxPort)
	}
	if c.HTTPPort < minUserPort || c.HTTPPort > maxPort {
		return fmt.Errorf("HTTP port must be between %d and %d", minUserPort, maxPort)
	}
	return nil
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > maxPort {
		return 0, fmt.Errorf("SMP port must be between 1 and %d", maxPort)
	}
	return port, nil
}

// validHost accepts an IP address or a DNS name and nothing else. The host is
// pasted straight into the SMP's URL, so a "/", "@", or "?" would quietly send
// requests, and the credentials with them, somewhere other than it appears.
func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
