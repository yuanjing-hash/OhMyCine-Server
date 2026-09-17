// Package browsercompanion controls only the bundled loopback browser helper.
package browsercompanion

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("browser_component_unavailable")

type Failure struct{ Code string }

func (e *Failure) Error() string { return e.Code }
func ErrorCode(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return "browser_component_unavailable"
}

type Caller interface {
	Call(context.Context, string, any, any) error
}

type Manager struct {
	mu                 sync.Mutex
	cmd                *exec.Cmd
	done               chan struct{}
	address, token     string
	node, script, data string
}

func New() *Manager {
	executable, _ := os.Executable()
	script := os.Getenv("OMC_CLOAK_COMPANION")
	if script == "" {
		script = filepath.Join(filepath.Dir(executable), "browser-companion", "src", "main.mjs")
	}
	node := os.Getenv("OMC_CLOAK_NODE")
	if node == "" {
		node = "node"
	}
	data := os.Getenv("OMC_CLOAK_DATA_DIR")
	if data == "" {
		root, _ := os.UserCacheDir()
		data = filepath.Join(root, "OhMyCine", "browser")
	}
	return &Manager{node: node, script: script, data: data}
}

// start never accepts a license or installs a browser. stdout/stderr are not
// forwarded: third-party errors can include page URLs and credentials.
func (m *Manager) start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		select {
		case <-m.done:
			m.cmd = nil
		default:
			return nil
		}
	}
	if !filepath.IsAbs(m.script) || !filepath.IsAbs(m.data) {
		return ErrUnavailable
	}
	if _, err := os.Stat(m.script); err != nil {
		return &Failure{Code: "browser_companion_missing"}
	}
	if _, err := exec.LookPath(m.node); err != nil {
		return &Failure{Code: "browser_node_missing"}
	}
	if err := os.MkdirAll(m.data, 0700); err != nil {
		return ErrUnavailable
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return ErrUnavailable
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return ErrUnavailable
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	m.token = hex.EncodeToString(secret)
	m.address = "http://127.0.0.1:" + strconv.Itoa(port)
	command := exec.Command(m.node, m.script)
	command.Env = append(browserEnvironment(), "OMC_CLOAK_PORT="+strconv.Itoa(port), "OMC_CLOAK_TOKEN="+m.token, "OMC_CLOAK_DATA_DIR="+m.data)
	hideWindow(command)
	if err := command.Start(); err != nil {
		return ErrUnavailable
	}
	m.cmd = command
	m.done = make(chan struct{})
	done := m.done
	go func() { _ = command.Wait(); close(done) }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			terminateTree(command)
			return ctx.Err()
		case <-done:
			return ErrUnavailable
		case <-deadline.C:
			terminateTree(command)
			return ErrUnavailable
		case <-tick.C:
			var response struct {
				ProtocolVersion int `json:"protocolVersion"`
			}
			if m.call(ctx, "status", struct{}{}, &response) == nil && response.ProtocolVersion == 1 {
				return nil
			}
		}
	}
}

// Do not inherit Server keys/tokens, ambient proxies or NODE_OPTIONS into the
// helper. Only platform runtime locations are needed by Node and Chromium.
func browserEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "SYSTEMROOT": true, "WINDIR": true, "TEMP": true, "TMP": true, "TMPDIR": true, "HOME": true, "USERPROFILE": true, "LOCALAPPDATA": true, "APPDATA": true, "XDG_CACHE_HOME": true, "XDG_RUNTIME_DIR": true, "LD_LIBRARY_PATH": true, "LANG": true, "LC_ALL": true}
	var result []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allowed[strings.ToUpper(key)] {
			result = append(result, entry)
		}
	}
	// This single deployment-admin flag does not authorize ambient proxy URLs,
	// arbitrary private ranges or guest-provided routing configuration.
	result = append(result, "OMC_CLOAK_TUN_FAKE_IP="+strconv.FormatBool(os.Getenv("OMC_CLOAK_TUN_FAKE_IP") == "true"))
	return result
}

func (m *Manager) Call(ctx context.Context, operation string, input, output any) error {
	if err := m.start(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.call(ctx, operation, input, output)
}

func (m *Manager) call(ctx context.Context, operation string, input, output any) error {
	switch operation {
	case "status", "install", "session/create", "session/snapshot", "session/reload", "session/input", "session/request", "session/cookies", "session/navigate", "session/close", "shutdown":
	default:
		return ErrUnavailable
	}
	data, err := json.Marshal(input)
	if err != nil || len(data) > 300<<10 {
		return ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.address+"/v1/"+operation, bytes.NewReader(data))
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+m.token)
	request.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	timeout := 35 * time.Second
	if operation == "session/create" {
		// DNS, process launch and initial navigation each have bounded deadlines.
		timeout = 70 * time.Second
	}
	client := http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return ErrUnavailable
	}
	if response.StatusCode != 200 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &failure) == nil {
			switch failure.Error {
			case "browser_request_failed", "browser_request_timeout", "browser_not_installed", "license_required", "browser_launch_failed", "browser_navigation_failed", "browser_reload_post_denied", "network_denied", "network_timeout", "resource_network_denied", "resource_network_timeout", "resource_network_failed", "resource_limit_exceeded", "tun_fake_ip_requires_opt_in", "session_expired":
				return &Failure{Code: failure.Error}
			}
		}
		return ErrUnavailable
	}
	if output == nil {
		return nil
	}
	if json.Unmarshal(body, output) != nil {
		return ErrUnavailable
	}
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		select {
		case <-m.done:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.call(ctx, "shutdown", struct{}{}, nil)
		select {
		case <-m.done:
		case <-ctx.Done():
			terminateTree(m.cmd)
		}
	}
}
