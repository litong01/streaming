package preview

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"streaming/internal/config"
)

const StreamName = "smp"

type Manager struct {
	mu     sync.Mutex
	binary string
	home   string
	cmd    *exec.Cmd
	yaml   string
	ready  bool
}

func New() *Manager {
	home := os.Getenv("STREAMING_GO2RTC_HOME")
	if home == "" {
		if cfg, err := config.Path(); err == nil {
			home = filepath.Join(filepath.Dir(cfg), "go2rtc")
		} else {
			home = filepath.Join(os.TempDir(), "streaming-go2rtc")
		}
	}
	return &Manager{
		binary: resolveBinary(),
		home:   home,
	}
}

func resolveBinary() string {
	if env := os.Getenv("STREAMING_GO2RTC"); env != "" {
		return env
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "go2rtc")
		if fileExists(candidate) {
			return candidate
		}
		candidate = filepath.Join(filepath.Dir(exe), "libgo2rtc.so")
		if fileExists(candidate) {
			return candidate
		}
	}
	if path, err := exec.LookPath("go2rtc"); err == nil {
		return path
	}
	return ""
}

func (m *Manager) Available() bool {
	return m != nil && m.binary != "" && fileExists(m.binary)
}

func (m *Manager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *Manager) Sync(cfg config.Config) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.Available() || cfg.PreviewURL == "" {
		m.stopLocked()
		return
	}

	yaml := renderConfig(cfg)
	if m.cmd != nil && m.yaml == yaml && processAlive(m.cmd) {
		m.ready = probe(cfg.Go2rtcPort)
		return
	}

	m.stopLocked()
	if err := os.MkdirAll(m.home, 0o700); err != nil {
		log.Printf("go2rtc home: %v", err)
		return
	}
	configPath := filepath.Join(m.home, "go2rtc.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		log.Printf("go2rtc config: %v", err)
		return
	}

	cmd := exec.Command(m.binary, "-config", configPath)
	cmd.Dir = m.home
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		log.Printf("start go2rtc: %v", err)
		return
	}
	m.cmd = cmd
	m.yaml = yaml
	log.Printf("go2rtc listening on http://0.0.0.0:%d/", cfg.Go2rtcPort)

	go func(process *exec.Cmd) {
		_ = process.Wait()
		m.mu.Lock()
		if m.cmd == process {
			m.cmd = nil
			m.ready = false
			m.yaml = ""
		}
		m.mu.Unlock()
	}(cmd)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probe(cfg.Go2rtcPort) {
			m.ready = true
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	m.ready = false
	log.Printf("go2rtc did not become ready on port %d", cfg.Go2rtcPort)
}

func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *Manager) stopLocked() {
	cmd := m.cmd
	if cmd == nil || cmd.Process == nil {
		m.cmd = nil
		m.ready = false
		m.yaml = ""
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cmd.ProcessState == nil {
		_ = cmd.Process.Kill()
		deadline = time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) && cmd.ProcessState == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	m.cmd = nil
	m.ready = false
	m.yaml = ""
}

func renderConfig(cfg config.Config) string {
	port := cfg.Go2rtcPort
	if port == 0 {
		port = config.DefaultGo2rtcPort
	}
	return fmt.Sprintf(`api:
  listen: ":%d"
  origin: "*"
log:
  level: info
rtsp:
  listen: ""
webrtc:
  listen: ":8555"
streams:
  %s: %s
`, port, StreamName, strconv.Quote(cfg.PreviewURL))
}

func probe(port int) bool {
	if port <= 0 {
		return false
	}
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

func processAlive(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.Process != nil && cmd.ProcessState == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
