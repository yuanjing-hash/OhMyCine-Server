package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeagent"
)

func main() {
	if err := runPlatform(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, safeErrorCode(err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := nodeagent.LoadConfigFromEnvironment()
	if err != nil {
		return err
	}
	if !cfg.AllowInsecureDevelopment {
		if err := nodeagent.EnsureTLSIdentity(cfg); err != nil {
			return err
		}
		if err := nodeagent.EnsureSealingIdentity(cfg); err != nil {
			return err
		}
	}
	store, err := nodeagent.OpenStore(filepath.Join(cfg.DataDirectory, "node.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	agent, err := nodeagent.New(cfg, store)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.ListenAddress, Handler: agent.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	if !cfg.AllowInsecureDevelopment && cfg.Transport == "https" {
		tlsConfig, err := tlsConfig(cfg)
		if err != nil {
			return err
		}
		server.TLSConfig = tlsConfig
	}
	errCh := make(chan error, 1)
	go func() {
		if cfg.AllowInsecureDevelopment || cfg.Transport == "http" {
			errCh <- server.ListenAndServe()
			return
		}
		errCh <- server.ListenAndServeTLS(cfg.TLSCertificateFile, cfg.TLSPrivateKeyFile)
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, done := contextWithTimeout()
		defer done()
		return server.Shutdown(shutdownCtx)
	}
	return nil
}

func tlsConfig(cfg nodeagent.Config) (*tls.Config, error) {
	result := &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert}
	if cfg.ClientCAFile == "" {
		return result, nil
	}
	caBytes, err := os.ReadFile(cfg.ClientCAFile)
	if err != nil {
		return nil, errors.New("node_client_ca_unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("node_client_ca_invalid")
	}
	result.ClientCAs = pool
	return result, nil
}

func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
func safeErrorCode(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 96 || strings.ContainsAny(value, " /\\:\r\n\t") {
		return "node_start_failed"
	}
	return value
}
