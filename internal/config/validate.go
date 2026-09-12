package config

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

const (
	minUserPort = 1024
	maxPort     = 65535
	minPreset   = 1
	maxPreset   = 32
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
		port = DefaultSmpSSHPort
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
	if host == "" || len(host) > maxHostLen {
		return "", 0, fmt.Errorf("invalid SMP host")
	}
	if port < 1 || port > maxPort {
		return "", 0, fmt.Errorf("SSH port must be between 1 and %d", maxPort)
	}
	return host, port, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.SmpHost) == "" || strings.TrimSpace(c.SmpUsername) == "" {
		return fmt.Errorf("SMP host and username are required")
	}
	if _, _, err := ParseHostPort(c.SmpHost, c.SmpSSHPort); err != nil {
		return err
	}
	username := strings.TrimSpace(c.SmpUsername)
	if len(username) > maxUserLen || containsControl(username) {
		return fmt.Errorf("invalid SSH username")
	}
	if c.SmpSSHPort < 1 || c.SmpSSHPort > maxPort {
		return fmt.Errorf("SSH port must be between 1 and %d", maxPort)
	}
	if c.HTTPPort < minUserPort || c.HTTPPort > maxPort {
		return fmt.Errorf("HTTP port must be between %d and %d", minUserPort, maxPort)
	}
	if c.EnglishPreset < minPreset || c.EnglishPreset > maxPreset ||
		c.MandarinPreset < minPreset || c.MandarinPreset > maxPreset {
		return fmt.Errorf("preset numbers must be between %d and %d", minPreset, maxPreset)
	}
	if c.EnglishPreset == c.MandarinPreset {
		return fmt.Errorf("English and Mandarin presets must be different")
	}
	return nil
}

func parsePort(value string) (int, error) {
	var port int
	if _, err := fmt.Sscanf(value, "%d", &port); err != nil || port < 1 || port > maxPort {
		return 0, fmt.Errorf("SSH port must be between 1 and %d", maxPort)
	}
	return port, nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
