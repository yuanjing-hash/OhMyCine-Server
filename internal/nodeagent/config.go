package nodeagent

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type Config struct {
	NodeID                   string
	ListenAddress            string
	DataDirectory            string
	TLSCertificateFile       string
	TLSPrivateKeyFile        string
	SealingPrivateKeyFile    string
	ClientCAFile             string
	EnrollmentToken          string
	AllowInsecureDevelopment bool
	Transport                string // http or https; HTTP still requires enrollment/signing
	ManagedRoot              string
	MaxConcurrentOperations  int
}

func LoadConfigFromEnvironment() (Config, error) {
	dataDir := strings.TrimSpace(os.Getenv("OMC_NODE_DATA_DIR"))
	if dataDir == "" {
		dataDir = filepath.Join("data", "node")
	}
	concurrency := 2
	if raw := strings.TrimSpace(os.Getenv("OMC_NODE_MAX_CONCURRENT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 32 {
			return Config{}, errors.New("OMC_NODE_MAX_CONCURRENT must be between 1 and 32")
		}
		concurrency = parsed
	}
	cfg := Config{
		NodeID:                   strings.TrimSpace(os.Getenv("OMC_NODE_ID")),
		ListenAddress:            strings.TrimSpace(os.Getenv("OMC_NODE_LISTEN")),
		DataDirectory:            dataDir,
		TLSCertificateFile:       strings.TrimSpace(os.Getenv("OMC_NODE_TLS_CERT")),
		TLSPrivateKeyFile:        strings.TrimSpace(os.Getenv("OMC_NODE_TLS_KEY")),
		SealingPrivateKeyFile:    strings.TrimSpace(os.Getenv("OMC_NODE_SEAL_KEY")),
		ClientCAFile:             strings.TrimSpace(os.Getenv("OMC_NODE_CLIENT_CA")),
		EnrollmentToken:          strings.TrimSpace(os.Getenv("OMC_NODE_ENROLLMENT_TOKEN")),
		AllowInsecureDevelopment: strings.EqualFold(strings.TrimSpace(os.Getenv("OMC_NODE_INSECURE_DEV")), "true"),
		Transport:                strings.ToLower(strings.TrimSpace(os.Getenv("OMC_NODE_TRANSPORT"))),
		ManagedRoot:              strings.TrimSpace(os.Getenv("OMC_NODE_MANAGED_ROOT")),
		MaxConcurrentOperations:  concurrency,
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":4433"
	}
	if cfg.Transport == "" {
		cfg.Transport = "https"
	}
	if cfg.ManagedRoot == "" {
		cfg.ManagedRoot = filepath.Join(dataDir, "managed")
	}
	if cfg.TLSCertificateFile == "" {
		cfg.TLSCertificateFile = filepath.Join(dataDir, "node.crt")
	}
	if cfg.TLSPrivateKeyFile == "" {
		cfg.TLSPrivateKeyFile = filepath.Join(dataDir, "node.key")
	}
	if cfg.SealingPrivateKeyFile == "" {
		cfg.SealingPrivateKeyFile = filepath.Join(dataDir, "node.seal.key")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.NodeID == "" || len(c.NodeID) > 128 {
		return errors.New("OMC_NODE_ID is required")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return errors.New("OMC_NODE_LISTEN must be host:port")
	}
	if strings.TrimSpace(c.DataDirectory) == "" || strings.TrimSpace(c.ManagedRoot) == "" {
		return errors.New("node data and managed directories are required")
	}
	if c.MaxConcurrentOperations < 1 || c.MaxConcurrentOperations > 32 {
		return errors.New("node operation concurrency is invalid")
	}
	if c.Transport != "" && c.Transport != "http" && c.Transport != "https" {
		return errors.New("OMC_NODE_TRANSPORT must be http or https")
	}
	if c.AllowInsecureDevelopment {
		return nil
	}
	if c.TLSCertificateFile == "" || c.TLSPrivateKeyFile == "" || c.SealingPrivateKeyFile == "" {
		return errors.New("public node requires a TLS certificate and key")
	}
	return nil
}

func (c Config) Capabilities(freeBytes *int64) nodeprotocol.Capabilities {
	return nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilityQBittorrentControl, nodeprotocol.CapabilityRangeExport, nodeprotocol.CapabilityPan115Offline, nodeprotocol.CapabilityPan115ShareReceive, nodeprotocol.CapabilityPan115Read, nodeprotocol.CapabilityPan115Upload}, MaxConcurrent: c.MaxConcurrentOperations, ManagedFreeBytes: freeBytes, ManagedFreeBytesKnown: freeBytes != nil}.Canonical()
}

func Platform() (string, string) { return runtime.GOOS, runtime.GOARCH }
