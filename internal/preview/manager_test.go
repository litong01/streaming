package preview

import (
	"strings"
	"testing"

	"streaming/internal/config"
)

func TestRenderConfigQuotesStreamURL(t *testing.T) {
	yaml := renderConfig(config.Config{
		PreviewURL: "rtsp://192.0.2.10/extron2",
		Go2rtcPort: 1984,
	})
	if !strings.Contains(yaml, `smp: "rtsp://192.0.2.10/extron2"`) {
		t.Fatalf("unexpected yaml:\n%s", yaml)
	}
	if !strings.Contains(yaml, `listen: ":1984"`) {
		t.Fatalf("missing api listen:\n%s", yaml)
	}
}
