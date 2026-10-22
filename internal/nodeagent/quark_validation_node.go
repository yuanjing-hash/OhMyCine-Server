//go:build quark_live

package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// Options contain no cloud credentials. The parent sends only sealed access
// grants through the ordinary signed Node protocol after enrollment.
type QuarkValidationNodeOptions struct {
	NodeID             string `json:"node_id"`
	ServerID           string `json:"server_id"`
	TaskID             string `json:"task_id"`
	ExportOperationKey string `json:"export_operation_key"`
	DataDirectory      string `json:"data_directory"`
	ManagedRoot        string `json:"managed_root"`
	SourceName         string `json:"source_name"`
	SourceSize         int64  `json:"source_size"`
	SourceSHA256       string `json:"source_sha256"`
	EnrollmentToken    string `json:"enrollment_token"`
}

type QuarkValidationNodeReady struct {
	Socket   string                              `json:"socket"`
	PID      int                                 `json:"pid"`
	Manifest nodeprotocol.FileExportManifestPage `json:"manifest"`
}

// The caller must supply a separately reviewed durable cloud-budget wrapper.
// A nil/error wrapper fails closed. This helper supplies no permissive default.
type QuarkValidationCloudWrapper func(cloud.Config, http.RoundTripper, bool) (http.RoundTripper, error)

// RunQuarkValidationNode is intended to run inside a dedicated OS child, using
// the real Node Store/Handler and signature/sealing protocol. It is absent from
// ordinary binaries and does not alter production origin or enrollment policy.
func RunQuarkValidationNode(ctx context.Context, o QuarkValidationNodeOptions, wrap QuarkValidationCloudWrapper, ready func(QuarkValidationNodeReady) error) error {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) || time.Until(deadline) > time.Hour || wrap == nil || ready == nil {
		return errors.New("quark_node_validation_lifetime_or_guard_invalid")
	}
	if err := validateQuarkValidationNodeOptions(o); err != nil {
		return err
	}
	ctx, end := context.WithCancel(ctx)
	defer end()
	if err := prepareQuarkValidationDirectory(o); err != nil {
		return err
	}
	cfg := Config{NodeID: o.NodeID, Transport: "http", ListenAddress: "127.0.0.1:0", DataDirectory: o.DataDirectory, ManagedRoot: o.ManagedRoot, MaxConcurrentOperations: 1, EnrollmentToken: o.EnrollmentToken, TLSCertificateFile: filepath.Join(o.DataDirectory, "node.crt"), TLSPrivateKeyFile: filepath.Join(o.DataDirectory, "node.key"), SealingPrivateKeyFile: filepath.Join(o.DataDirectory, "node.seal.key")}
	if err := EnsureTLSIdentity(cfg); err != nil {
		return errors.New("quark_node_validation_identity_failed")
	}
	if err := EnsureSealingIdentity(cfg); err != nil {
		return errors.New("quark_node_validation_identity_failed")
	}
	store, err := OpenStore(filepath.Join(o.DataDirectory, "node.db"))
	if err != nil {
		return errors.New("quark_node_validation_store_failed")
	}
	closeStore := true
	defer func() {
		if closeStore {
			_ = store.Close()
		}
	}()
	agent, err := New(cfg, store)
	if err != nil {
		return errors.New("quark_node_validation_agent_failed")
	}
	agent.quarkClientFactory = func(c cloud.Config) (*quark.Client, error) {
		var guardErr error
		client, err := quark.NewObservationValidationClient(c, func(base http.RoundTripper, media bool) http.RoundTripper {
			wrapped, wrapErr := wrap(c, base, media)
			if wrapErr != nil || wrapped == nil {
				guardErr = errors.New("quark_node_validation_cloud_guard_failed")
				return quarkValidationDenyTransport{}
			}
			return &quarkValidationLifetimeTransport{ctx: ctx, base: wrapped}
		})
		if guardErr != nil {
			return nil, guardErr
		}
		return client, err
	}
	// The experiment is Quark-only. No unbudgeted 115 or source/downloader action
	// can be reached through this helper, even by the paired fixture controller.
	agent.storageFactory = func(nodeprotocol.StorageCredential, func(context.Context) error) (storageUploadDriver, error) {
		return nil, errors.New(nodeprotocol.ErrorCapabilityMissing)
	}
	manifest, err := prepareQuarkValidationSource(ctx, agent, o, deadline)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("quark_node_validation_listen_failed")
	}
	defer listener.Close()
	handler := agent.Handler()
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/node/v1/downloader/") || strings.HasPrefix(r.URL.Path, "/node/v1/storage-source/") || strings.HasSuffix(r.URL.Path, "/cleanup") {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	if err = ready(QuarkValidationNodeReady{Socket: listener.Addr().String(), PID: os.Getpid(), Manifest: manifest}); err != nil {
		_ = server.Close()
		<-serveDone
		return errors.New("quark_node_validation_ready_failed")
	}
	serveStopped := false
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveDone:
		serveStopped = true
	}
	end()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = server.Shutdown(shutdown)
	cancel()
	_ = server.Close()
	if !serveStopped {
		<-serveDone
	}
	// Accepted upload goroutines run independently of HTTP requests. Cancellation
	// reaches their actual native API/media transports, then we wait for receipt
	// persistence before closing SQLite. Never inspect the live database outside.
	until := time.Now().Add(30 * time.Second)
	for {
		agent.storageMu.Lock()
		running := len(agent.storageRuns)
		agent.storageMu.Unlock()
		if running == 0 {
			break
		}
		if time.Now().After(until) {
			closeStore = false
			return errors.New("quark_node_validation_quiescence_timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = store.Close(); err != nil {
		return errors.New("quark_node_validation_store_close_failed")
	}
	closeStore = false
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.New("quark_node_validation_listener_failed")
	}
	return nil
}

func validateQuarkValidationNodeOptions(o QuarkValidationNodeOptions) error {
	for _, id := range []string{o.NodeID, o.ServerID, o.TaskID, o.ExportOperationKey} {
		if !strings.HasPrefix(id, "quark-validation-") || !validEnrollmentID(id) {
			return errors.New("quark_node_validation_identity_invalid")
		}
	}
	if !filepath.IsAbs(o.DataDirectory) || !filepath.IsAbs(o.ManagedRoot) || o.SourceName == "" || filepath.Base(o.SourceName) != o.SourceName || strings.ContainsAny(o.SourceName, "/\\") || o.SourceSize <= 0 || o.SourceSize > (2<<30)+1 || !nodeprotocol.ValidDigest(o.SourceSHA256) || len(o.EnrollmentToken) < 32 || len(o.EnrollmentToken) > 128 {
		return errors.New("quark_node_validation_fixture_invalid")
	}
	// State and the generated source are distinct siblings in one isolated
	// batch. The parent owns the batch ACL; no live service directory is reused.
	if sameCleanPath(o.DataDirectory, o.ManagedRoot) || !sameCleanPath(filepath.Dir(o.DataDirectory), filepath.Dir(o.ManagedRoot)) {
		return errors.New("quark_node_validation_directory_invalid")
	}
	resolved, err := filepath.EvalSymlinks(o.ManagedRoot)
	if err != nil || !sameCleanPath(resolved, o.ManagedRoot) {
		return errors.New("quark_node_validation_source_directory_invalid")
	}
	return nil
}

func prepareQuarkValidationDirectory(o QuarkValidationNodeOptions) error {
	if err := os.MkdirAll(o.DataDirectory, 0700); err != nil {
		return errors.New("quark_node_validation_directory_failed")
	}
	resolved, err := filepath.EvalSymlinks(o.DataDirectory)
	if err != nil || !sameCleanPath(resolved, o.DataDirectory) {
		return errors.New("quark_node_validation_directory_invalid")
	}
	// A resumed owned batch must not redirect its marker, keys, or SQLite files
	// outside this directory. The enclosing private ACL is still a prerequisite.
	for _, name := range []string{"quark-validation-owner.json", "node.db", "node.db-wal", "node.db-shm", "node.crt", "node.key", "node.seal.key"} {
		file := filepath.Join(o.DataDirectory, name)
		info, statErr := os.Lstat(file)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() {
			return errors.New("quark_node_validation_state_file_invalid")
		}
		resolved, resolveErr := filepath.EvalSymlinks(file)
		if resolveErr != nil || !sameCleanPath(resolved, file) {
			return errors.New("quark_node_validation_state_file_invalid")
		}
	}
	marker := o
	marker.EnrollmentToken = ""
	raw, _ := json.Marshal(marker)
	ownerPath := filepath.Join(o.DataDirectory, "quark-validation-owner.json")
	prior, err := os.ReadFile(ownerPath)
	if err == nil {
		if !bytes.Equal(raw, prior) {
			return errors.New("quark_node_validation_owner_mismatch")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("quark_node_validation_owner_unreadable")
	}
	entries, err := os.ReadDir(o.DataDirectory)
	if err != nil || len(entries) != 0 {
		return errors.New("quark_node_validation_directory_not_fresh")
	}
	if err = writeExclusive(ownerPath, raw, 0600); err != nil {
		return errors.New("quark_node_validation_owner_failed")
	}
	return nil
}

func prepareQuarkValidationSource(ctx context.Context, a *Agent, o QuarkValidationNodeOptions, deadline time.Time) (nodeprotocol.FileExportManifestPage, error) {
	var empty nodeprotocol.FileExportManifestPage
	record, err := buildFileExport(ctx, o.ManagedRoot, o.ServerID, o.ExportOperationKey, ManagedDownload{TaskID: o.TaskID, NodeLocalRoot: o.ManagedRoot}, downloadpkg.Manifest{Complete: true, Name: "Quark Node validation fixture", Files: []downloadpkg.File{{RelativePath: o.SourceName, Size: o.SourceSize}}}, time.Now().UTC(), deadline)
	if err != nil || len(record.Files) != 1 || record.Files[0].Public.SHA256 != o.SourceSHA256 {
		return empty, errors.New("quark_node_validation_source_mismatch")
	}
	if err = a.store.SaveFileExport(ctx, o.ServerID, record); err != nil {
		return empty, errors.New("quark_node_validation_export_failed")
	}
	// On restart retain the original exported token and manifest, not the fresh
	// random token produced while rehashing the same local file.
	if _, err = a.store.RenewFileExport(ctx, o.ServerID, o.ExportOperationKey, o.TaskID, time.Now().UTC(), deadline); err != nil {
		return empty, errors.New("quark_node_validation_export_expired")
	}
	return a.store.FileExportManifest(ctx, o.ServerID, o.ExportOperationKey, 1, 1, time.Now().UTC())
}

type quarkValidationDenyTransport struct{}

func (quarkValidationDenyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("quark_node_validation_cloud_guard_missing")
}

type quarkValidationLifetimeTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t *quarkValidationLifetimeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	if err := t.ctx.Err(); err != nil {
		stop()
		cancel()
		return nil, err
	}
	response, err := t.base.RoundTrip(r.Clone(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if response == nil || response.Body == nil {
		stop()
		cancel()
		return nil, errors.New("quark_node_validation_transport_invalid")
	}
	response.Body = &quarkValidationLifetimeBody{ReadCloser: response.Body, stop: func() { stop(); cancel() }}
	return response, nil
}

type quarkValidationLifetimeBody struct {
	io.ReadCloser
	once sync.Once
	stop func()
}

func (b *quarkValidationLifetimeBody) Close() error { b.once.Do(b.stop); return b.ReadCloser.Close() }
