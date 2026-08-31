package updater

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
)

const (
	settingsSchemaVersion = 1
	stateSchemaVersion    = 1
	planSchemaVersion     = 1
	maxJSONFileBytes      = 1 << 20
)

type Phase string

const (
	PhaseIdle           Phase = "idle"
	PhaseChecking       Phase = "checking"
	PhaseAvailable      Phase = "available"
	PhaseDownloading    Phase = "downloading"
	PhaseReady          Phase = "ready"
	PhaseWaitingForExit Phase = "waiting_for_exit"
	PhaseReplacing      Phase = "replacing"
	PhaseRestarting     Phase = "restarting"
	PhaseVerifying      Phase = "verifying"
	PhaseSucceeded      Phase = "succeeded"
	PhaseFailed         Phase = "failed"
	PhaseRolledBack     Phase = "rolled_back"
)

var operationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var errorCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,96}$`)

func (p Phase) Valid() bool {
	switch p {
	case PhaseIdle, PhaseChecking, PhaseAvailable, PhaseDownloading, PhaseReady, PhaseWaitingForExit, PhaseReplacing, PhaseRestarting, PhaseVerifying, PhaseSucceeded, PhaseFailed, PhaseRolledBack:
		return true
	default:
		return false
	}
}

type Settings struct {
	SchemaVersion int       `json:"schema_version"`
	Channel       Channel   `json:"channel"`
	Revision      uint64    `json:"revision"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type State struct {
	SchemaVersion  int        `json:"schema_version"`
	Phase          Phase      `json:"phase"`
	CurrentVersion string     `json:"current_version"`
	TargetVersion  string     `json:"target_version,omitempty"`
	Channel        Channel    `json:"channel"`
	OperationID    string     `json:"operation_id,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// Plan is private helper input and must never be returned by an API.
type Plan struct {
	SchemaVersion     int       `json:"schema_version"`
	OperationID       string    `json:"operation_id"`
	TargetVersion     string    `json:"target_version"`
	RuntimeDirectory  string    `json:"runtime_directory"`
	CurrentExecutable string    `json:"current_executable"`
	CurrentSHA256     string    `json:"current_sha256"`
	Candidate         string    `json:"candidate"`
	Backup            string    `json:"backup"`
	ParentPID         int       `json:"parent_pid"`
	OriginalArgs      []string  `json:"original_args"`
	HealthURL         string    `json:"health_url"`
	ParentWaitMillis  int64     `json:"parent_wait_millis"`
	HealthWaitMillis  int64     `json:"health_wait_millis"`
	CreatedAt         time.Time `json:"created_at"`
}

type OperationPaths struct {
	OperationID string
	Directory   string
	Archive     string
	Candidate   string
	Helper      string
	Backup      string
	Plan        string
}

type Store struct {
	runtimeRoot string
	root        string
	mu          sync.Mutex
}

func NewStore(runtimeDirectory string) (*Store, error) {
	abs, err := filepath.Abs(runtimeDirectory)
	if err != nil {
		return nil, coded(CodePersistence, err)
	}
	abs = filepath.Clean(abs)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, coded(CodePersistence, err)
	}
	resolvedRuntime, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, coded(CodePersistence, err)
	}
	abs = filepath.Clean(resolvedRuntime)
	root := filepath.Join(abs, "updates")
	for _, directory := range []string{root, filepath.Join(root, "staging"), filepath.Join(root, "plans"), filepath.Join(root, "backups")} {
		if info, err := os.Lstat(directory); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, coded(CodePersistence, errors.New("update directory is not a real directory"))
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, coded(CodePersistence, err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, coded(CodePersistence, err)
		}
	}
	return &Store{runtimeRoot: abs, root: root}, nil
}

func (s *Store) RuntimeDirectory() string { return s.runtimeRoot }
func (s *Store) Root() string             { return s.root }

func defaultSettings() Settings {
	return Settings{SchemaVersion: settingsSchemaVersion, Channel: ChannelBeta, Revision: 1}
}

func (s *Store) LoadSettings() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var settings Settings
	if err := readJSON(filepath.Join(s.root, "settings.json"), &settings); errors.Is(err, os.ErrNotExist) {
		return defaultSettings(), nil
	} else if err != nil {
		return Settings{}, coded(CodePersistence, err)
	}
	if settings.SchemaVersion != settingsSchemaVersion || !settings.Channel.Valid() || settings.Revision == 0 {
		return Settings{}, coded(CodePersistence, errors.New("update settings are invalid"))
	}
	return settings, nil
}

func (s *Store) UpdateSettings(channel Channel, expectedRevision uint64) (Settings, error) {
	if !channel.Valid() {
		return Settings{}, coded(CodeInvalidChannel, errors.New("unknown update channel"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := defaultSettings()
	if err := readJSON(filepath.Join(s.root, "settings.json"), &settings); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Settings{}, coded(CodePersistence, err)
	}
	if settings.SchemaVersion != settingsSchemaVersion || !settings.Channel.Valid() || settings.Revision == 0 {
		return Settings{}, coded(CodePersistence, errors.New("update settings are invalid"))
	}
	if expectedRevision == 0 || settings.Revision != expectedRevision {
		return Settings{}, coded(CodeRevisionConflict, errors.New("update settings revision changed"))
	}
	settings.Channel = channel
	settings.Revision++
	settings.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(filepath.Join(s.root, "settings.json"), settings, 0o600); err != nil {
		return Settings{}, coded(CodePersistence, err)
	}
	return settings, nil
}

func (s *Store) LoadState() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state State
	if err := readJSON(filepath.Join(s.root, "state.json"), &state); errors.Is(err, os.ErrNotExist) {
		info := buildinfo.Current()
		return State{SchemaVersion: stateSchemaVersion, Phase: PhaseIdle, CurrentVersion: info.Version, Channel: ChannelBeta}, nil
	} else if err != nil {
		return State{}, coded(CodePersistence, err)
	}
	if err := validateState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *Store) SaveState(state State) error {
	state.SchemaVersion = stateSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	if err := validateState(state); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicWriteJSON(filepath.Join(s.root, "state.json"), state, 0o600); err != nil {
		return coded(CodePersistence, err)
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != stateSchemaVersion || !state.Phase.Valid() || !state.Channel.Valid() {
		return coded(CodePersistence, errors.New("update state is invalid"))
	}
	if state.OperationID != "" && !operationIDPattern.MatchString(state.OperationID) {
		return coded(CodePersistence, errors.New("update state operation is invalid"))
	}
	if state.ErrorCode != "" && !errorCodePattern.MatchString(state.ErrorCode) {
		return coded(CodePersistence, errors.New("update state error code is invalid"))
	}
	if state.CurrentVersion != "dev" {
		if _, err := buildinfo.ParseVersion(state.CurrentVersion); err != nil {
			return coded(CodePersistence, errors.New("current version is invalid"))
		}
	}
	if state.TargetVersion != "" {
		if _, err := buildinfo.ParseVersion(state.TargetVersion); err != nil {
			return coded(CodePersistence, errors.New("target version is invalid"))
		}
	}
	return nil
}

func newOperationID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func (s *Store) CreateOperation(names PlatformAssets) (OperationPaths, error) {
	id, err := newOperationID()
	if err != nil {
		return OperationPaths{}, coded(CodePersistence, err)
	}
	directory := filepath.Join(s.root, "staging", id)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return OperationPaths{}, coded(CodePersistence, err)
	}
	helperName := "ohmycine-server-helper"
	if filepath.Ext(names.Binary) == ".exe" {
		helperName += ".exe"
	}
	return OperationPaths{OperationID: id, Directory: directory, Archive: filepath.Join(directory, names.Archive), Candidate: filepath.Join(directory, names.Binary+".candidate"), Helper: filepath.Join(directory, helperName), Backup: filepath.Join(s.root, "backups", names.Binary+"-"+id+".bak"), Plan: filepath.Join(s.root, "plans", id+".json")}, nil
}

func (s *Store) SavePlan(plan Plan) (string, error) {
	plan.SchemaVersion = planSchemaVersion
	plan.RuntimeDirectory = s.runtimeRoot
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = time.Now().UTC()
	}
	if err := validatePlan(plan, s); err != nil {
		return "", err
	}
	planPath := filepath.Join(s.root, "plans", plan.OperationID+".json")
	if err := atomicWriteJSON(planPath, plan, 0o600); err != nil {
		return "", coded(CodePersistence, err)
	}
	return planPath, nil
}

func (s *Store) LoadPlan(operationID string) (Plan, error) {
	if !operationIDPattern.MatchString(operationID) {
		return Plan{}, coded(CodePlanInvalid, errors.New("operation ID is invalid"))
	}
	return LoadPlanFile(filepath.Join(s.root, "plans", operationID+".json"))
}

func LoadPlanFile(planPath string) (Plan, error) {
	var plan Plan
	if err := readJSON(planPath, &plan); err != nil {
		return Plan{}, coded(CodePlanInvalid, errors.New("helper plan cannot be read"))
	}
	runtimeRoot, err := filepath.Abs(plan.RuntimeDirectory)
	if err != nil {
		return Plan{}, coded(CodePlanInvalid, errors.New("helper runtime root is invalid"))
	}
	runtimeRoot, err = filepath.EvalSymlinks(runtimeRoot)
	if err != nil {
		return Plan{}, coded(CodePlanInvalid, errors.New("helper runtime root cannot be resolved"))
	}
	store := &Store{runtimeRoot: filepath.Clean(runtimeRoot), root: filepath.Join(filepath.Clean(runtimeRoot), "updates")}
	if filepath.Clean(planPath) != filepath.Join(store.root, "plans", plan.OperationID+".json") {
		return Plan{}, coded(CodePlanInvalid, errors.New("helper plan path is outside the update root"))
	}
	if err := validatePlan(plan, store); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func validatePlan(plan Plan, store *Store) error {
	if plan.SchemaVersion != planSchemaVersion || !operationIDPattern.MatchString(plan.OperationID) || plan.ParentPID <= 0 {
		return coded(CodePlanInvalid, errors.New("helper plan identity is invalid"))
	}
	if _, err := buildinfo.ParseVersion(plan.TargetVersion); err != nil {
		return coded(CodePlanInvalid, errors.New("helper target version is invalid"))
	}
	if len(plan.CurrentSHA256) != 64 {
		return coded(CodePlanInvalid, errors.New("installed executable digest is invalid"))
	}
	if _, err := hex.DecodeString(plan.CurrentSHA256); err != nil {
		return coded(CodePlanInvalid, errors.New("installed executable digest is invalid"))
	}
	if !samePath(plan.RuntimeDirectory, store.runtimeRoot) {
		return coded(CodePlanInvalid, errors.New("helper runtime root is invalid"))
	}
	if plan.ParentWaitMillis < 1000 || plan.ParentWaitMillis > int64((10*time.Minute)/time.Millisecond) || plan.HealthWaitMillis < 1000 || plan.HealthWaitMillis > int64((10*time.Minute)/time.Millisecond) {
		return coded(CodePlanInvalid, errors.New("helper plan timing is invalid"))
	}
	for _, value := range []string{plan.CurrentExecutable, plan.Candidate, plan.Backup} {
		if !filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
			return coded(CodePlanInvalid, errors.New("helper plan path is invalid"))
		}
	}
	if pathWithin(store.root, plan.CurrentExecutable) || !pathWithin(filepath.Join(store.root, "staging", plan.OperationID), plan.Candidate) || !pathWithin(filepath.Join(store.root, "backups"), plan.Backup) {
		return coded(CodePlanInvalid, errors.New("helper plan path crosses update boundaries"))
	}
	currentName := filepath.Base(plan.CurrentExecutable)
	if currentName != "ohmycine-server" && currentName != "ohmycine-server.exe" {
		return coded(CodePlanInvalid, errors.New("installed executable name is invalid"))
	}
	if filepath.Base(plan.Candidate) != currentName+".candidate" || !strings.HasPrefix(filepath.Base(plan.Backup), currentName+"-") || !strings.HasSuffix(filepath.Base(plan.Backup), ".bak") {
		return coded(CodePlanInvalid, errors.New("candidate or backup identity is invalid"))
	}
	if err := validateResolvedPlanPaths(store, plan); err != nil {
		return err
	}
	if len(plan.OriginalArgs) > 128 {
		return coded(CodePlanInvalid, errors.New("helper argument list is too large"))
	}
	for _, argument := range plan.OriginalArgs {
		if len(argument) > 4096 || strings.ContainsRune(argument, '\x00') || argument == HelperFlag {
			return coded(CodePlanInvalid, errors.New("helper argument list is invalid"))
		}
	}
	parsed, err := url.Parse(plan.HealthURL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/api/v1/health" {
		return coded(CodePlanInvalid, errors.New("helper health URL is invalid"))
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return coded(CodePlanInvalid, errors.New("helper health URL is not loopback"))
	}
	return nil
}

func validateResolvedPlanPaths(store *Store, plan Plan) error {
	resolvedRuntime, err := filepath.EvalSymlinks(store.runtimeRoot)
	if err != nil {
		return coded(CodePlanInvalid, errors.New("runtime root cannot be resolved"))
	}
	resolvedUpdates, err := filepath.EvalSymlinks(store.root)
	if err != nil || !pathWithin(resolvedRuntime, resolvedUpdates) {
		return coded(CodePlanInvalid, errors.New("update root cannot be resolved safely"))
	}
	resolvedCurrent, err := filepath.EvalSymlinks(plan.CurrentExecutable)
	if err != nil || pathWithin(resolvedUpdates, resolvedCurrent) {
		return coded(CodePlanInvalid, errors.New("installed executable cannot be resolved safely"))
	}
	resolvedCandidate, err := filepath.EvalSymlinks(plan.Candidate)
	if err != nil || !pathWithin(resolvedUpdates, resolvedCandidate) {
		return coded(CodePlanInvalid, errors.New("candidate cannot be resolved safely"))
	}
	resolvedBackupParent, err := filepath.EvalSymlinks(filepath.Dir(plan.Backup))
	if err != nil || !pathWithin(resolvedUpdates, resolvedBackupParent) {
		return coded(CodePlanInvalid, errors.New("backup directory cannot be resolved safely"))
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err != nil {
		return err
	} else if info.Size() > maxJSONFileBytes {
		return errors.New("JSON file exceeds size limit")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxJSONFileBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON file contains trailing data")
	}
	return nil
}

func atomicWriteJSON(path string, value any, mode os.FileMode) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".update-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, bytes.NewReader(payload)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return err
	}
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}
