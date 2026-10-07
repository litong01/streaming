package avcontrol

import (
	"os"
	"path/filepath"
)

// ResolvePath finds the AV control file without a second server.
// An explicit AVCONTROL_CONFIG wins. Otherwise a file next to the working
// directory's avcontrol/avcontrol.yaml is the development copy, and the
// Android build looks beside STREAMING_CONFIG so the same process that
// serves the streaming page also sees the rack file.
func ResolvePath() string {
	if path := stringsTrim(os.Getenv("AVCONTROL_CONFIG")); path != "" {
		return path
	}
	local := filepath.Join("avcontrol", "avcontrol.yaml")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if streaming := stringsTrim(os.Getenv("STREAMING_CONFIG")); streaming != "" {
		return filepath.Join(filepath.Dir(streaming), "avcontrol.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return local
	}
	return filepath.Join(home, ".config", "streaming", "avcontrol.yaml")
}

func stringsTrim(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return value
}
