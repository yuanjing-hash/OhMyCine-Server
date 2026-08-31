package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const HelperFlag = "--ohmycine-update-helper"

type PrepareRequest struct {
	CurrentExecutable string
	ParentPID         int
	OriginalArgs      []string
	HealthURL         string
	ParentWait        time.Duration
	HealthWait        time.Duration
}

type PreparedUpdate struct {
	OperationID      string
	Version          string
	HelperExecutable string
	PlanPath         string
}

// Prepare downloads and verifies the official archive, extracts the candidate,
// creates a distinct helper executable for Windows file-lock safety, and writes
// the private handoff plan. It never changes the installed executable.
func (c *GitHubClient) Prepare(ctx context.Context, release SelectedRelease, store *Store, request PrepareRequest) (PreparedUpdate, error) {
	if c == nil || store == nil {
		return PreparedUpdate{}, coded(CodePlanInvalid, errors.New("updater dependencies are missing"))
	}
	names, err := AssetNames(release.Version.String(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return PreparedUpdate{}, err
	}
	validated, err := ValidateRelease(Release{TagName: release.TagName, Prerelease: release.Prerelease, PublishedAt: release.PublishedAt, Assets: []Asset{release.Archive, release.Checksum}}, runtime.GOOS, runtime.GOARCH)
	if err != nil || validated.Version.Compare(release.Version) != 0 {
		return PreparedUpdate{}, coded(CodeInvalidRelease, errors.New("selected release identity is inconsistent"))
	}
	currentExecutable, err := filepath.Abs(request.CurrentExecutable)
	if err != nil {
		return PreparedUpdate{}, coded(CodePlanInvalid, errors.New("current executable path is invalid"))
	}
	info, err := os.Stat(currentExecutable)
	if err != nil || !info.Mode().IsRegular() {
		return PreparedUpdate{}, coded(CodePlanInvalid, errors.New("current executable is not a regular file"))
	}
	currentDigest, err := hashFile(currentExecutable, MaxCandidateBytes)
	if err != nil {
		return PreparedUpdate{}, err
	}
	paths, err := store.CreateOperation(names)
	if err != nil {
		return PreparedUpdate{}, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.RemoveAll(paths.Directory)
		}
	}()
	if err := c.DownloadAndVerify(ctx, release, paths.Archive); err != nil {
		return PreparedUpdate{}, err
	}
	if err := ExtractCandidate(paths.Archive, paths.Candidate, names); err != nil {
		return PreparedUpdate{}, err
	}
	if err := copyExecutable(paths.Candidate, paths.Helper); err != nil {
		return PreparedUpdate{}, err
	}
	parentWait := request.ParentWait
	if parentWait == 0 {
		parentWait = 90 * time.Second
	}
	healthWait := request.HealthWait
	if healthWait == 0 {
		healthWait = 90 * time.Second
	}
	plan := Plan{OperationID: paths.OperationID, TargetVersion: release.Version.String(), RuntimeDirectory: store.RuntimeDirectory(), CurrentExecutable: currentExecutable, CurrentSHA256: currentDigest, Candidate: paths.Candidate, Backup: paths.Backup, ParentPID: request.ParentPID, OriginalArgs: append([]string(nil), request.OriginalArgs...), HealthURL: request.HealthURL, ParentWaitMillis: parentWait.Milliseconds(), HealthWaitMillis: healthWait.Milliseconds(), CreatedAt: time.Now().UTC()}
	planPath, err := store.SavePlan(plan)
	if err != nil {
		return PreparedUpdate{}, err
	}
	succeeded = true
	return PreparedUpdate{OperationID: paths.OperationID, Version: release.Version.String(), HelperExecutable: paths.Helper, PlanPath: planPath}, nil
}

func hashFile(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", coded(CodePersistence, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, limit+1))
	if err != nil || written <= 0 || written > limit {
		return "", coded(CodePlanInvalid, errors.New("installed executable size is invalid"))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return coded(CodePersistence, err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return coded(CodePersistence, err)
	}
	succeeded := false
	defer func() {
		_ = output.Close()
		if !succeeded {
			_ = os.Remove(destination)
		}
	}()
	written, err := io.Copy(output, io.LimitReader(input, MaxCandidateBytes+1))
	if err != nil || written <= 0 || written > MaxCandidateBytes {
		return coded(CodePersistence, errors.New("helper executable copy failed"))
	}
	if err := output.Sync(); err != nil {
		return coded(CodePersistence, err)
	}
	if err := output.Close(); err != nil {
		return coded(CodePersistence, err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return coded(CodePersistence, err)
	}
	succeeded = true
	return nil
}
