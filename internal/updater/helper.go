package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

type RunningProcess interface {
	Kill() error
	Wait() error
	Release() error
}

type HelperOptions struct {
	WaitForExit       func(context.Context, int) error
	ResolveExecutable func(int) (string, error)
	Rename            func(string, string) error
	Remove            func(string) error
	Start             func(string, []string) (RunningProcess, error)
	Probe             func(context.Context, string) error
	Now               func() time.Time
}

type commandProcess struct{ command *exec.Cmd }

func (p *commandProcess) Kill() error    { return p.command.Process.Kill() }
func (p *commandProcess) Wait() error    { return p.command.Wait() }
func (p *commandProcess) Release() error { return p.command.Process.Release() }

func defaultStart(executable string, arguments []string) (RunningProcess, error) {
	command := exec.Command(executable, arguments...)
	command.Env = os.Environ()
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &commandProcess{command: command}, nil
}

func defaultProbe(ctx context.Context, healthURL string) error {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("health redirects are forbidden") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned status %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(payload) > 4096 {
		return errors.New("health response exceeds limit")
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&envelope); err != nil || envelope.Code != 0 || envelope.Data.Status != "ok" {
		return errors.New("health response is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("health response has trailing data")
	}
	return nil
}

func normalizeHelperOptions(options HelperOptions) HelperOptions {
	if options.WaitForExit == nil {
		options.WaitForExit = waitForProcessExit
	}
	if options.ResolveExecutable == nil {
		options.ResolveExecutable = processExecutable
	}
	if options.Rename == nil {
		options.Rename = os.Rename
	}
	if options.Remove == nil {
		options.Remove = os.Remove
	}
	if options.Start == nil {
		options.Start = defaultStart
	}
	if options.Probe == nil {
		options.Probe = defaultProbe
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return options
}

// RunHelper performs the private post-shutdown handoff. The helper executable
// must be a distinct copy from Plan.Candidate on Windows.
func RunHelper(ctx context.Context, planPath string, options HelperOptions) error {
	plan, err := LoadPlanFile(planPath)
	if err != nil {
		return err
	}
	options = normalizeHelperOptions(options)
	currentDigest, err := hashFile(plan.CurrentExecutable, MaxCandidateBytes)
	if err != nil || currentDigest != plan.CurrentSHA256 {
		return coded(CodePlanInvalid, errors.New("installed executable digest changed"))
	}
	parentExecutable, resolveErr := options.ResolveExecutable(plan.ParentPID)
	if resolveErr != nil || !samePath(parentExecutable, plan.CurrentExecutable) {
		return coded(CodePlanInvalid, errors.New("helper plan is not bound to the parent executable"))
	}
	store := &Store{runtimeRoot: filepath.Clean(plan.RuntimeDirectory), root: filepath.Join(filepath.Clean(plan.RuntimeDirectory), "updates")}
	state, err := store.LoadState()
	if err != nil {
		return err
	}
	state.OperationID = plan.OperationID
	state.TargetVersion = plan.TargetVersion
	state.ErrorCode = ""
	setPhase := func(phase Phase, errorCode string, completed bool) error {
		state.Phase = phase
		state.ErrorCode = errorCode
		now := options.Now().UTC()
		if state.StartedAt == nil {
			state.StartedAt = &now
		}
		if completed {
			state.CompletedAt = &now
		}
		return store.SaveState(state)
	}
	if err := setPhase(PhaseWaitingForExit, "", false); err != nil {
		return err
	}
	waitContext, cancelWait := context.WithTimeout(ctx, time.Duration(plan.ParentWaitMillis)*time.Millisecond)
	err = options.WaitForExit(waitContext, plan.ParentPID)
	cancelWait()
	if err != nil {
		updateErr := coded(CodeParentExitTimeout, err)
		_ = setPhase(PhaseFailed, ErrorCode(updateErr), true)
		return updateErr
	}
	restartUnchanged := func(cause error) error {
		oldProcess, startErr := options.Start(plan.CurrentExecutable, plan.OriginalArgs)
		if startErr != nil {
			rollbackErr := coded(CodeRollbackFailed, startErr)
			_ = setPhase(PhaseFailed, ErrorCode(rollbackErr), true)
			return rollbackErr
		}
		_ = oldProcess.Release()
		_ = setPhase(PhaseFailed, ErrorCode(cause), true)
		return cause
	}
	if err := setPhase(PhaseReplacing, "", false); err != nil {
		return restartUnchanged(err)
	}
	if _, err := os.Lstat(plan.Backup); err == nil || !errors.Is(err, os.ErrNotExist) {
		updateErr := coded(CodeReplacementFailed, errors.New("backup target already exists or cannot be inspected"))
		return restartUnchanged(updateErr)
	}
	if err := options.Rename(plan.CurrentExecutable, plan.Backup); err != nil {
		updateErr := coded(CodeReplacementFailed, err)
		return restartUnchanged(updateErr)
	}
	restore := func(cause error, running RunningProcess) error {
		if running != nil {
			_ = running.Kill()
			_ = running.Wait()
		}
		failedCandidate := plan.Candidate + ".failed"
		_ = options.Remove(failedCandidate)
		if err := options.Rename(plan.CurrentExecutable, failedCandidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErr := coded(CodeRollbackFailed, err)
			_ = setPhase(PhaseFailed, ErrorCode(rollbackErr), true)
			return rollbackErr
		}
		if err := options.Rename(plan.Backup, plan.CurrentExecutable); err != nil {
			rollbackErr := coded(CodeRollbackFailed, err)
			_ = setPhase(PhaseFailed, ErrorCode(rollbackErr), true)
			return rollbackErr
		}
		oldProcess, err := options.Start(plan.CurrentExecutable, plan.OriginalArgs)
		if err != nil {
			rollbackErr := coded(CodeRollbackFailed, err)
			_ = setPhase(PhaseFailed, ErrorCode(rollbackErr), true)
			return rollbackErr
		}
		_ = oldProcess.Release()
		_ = setPhase(PhaseRolledBack, ErrorCode(cause), true)
		return cause
	}
	if err := options.Rename(plan.Candidate, plan.CurrentExecutable); err != nil {
		return restore(coded(CodeReplacementFailed, err), nil)
	}
	if err := setPhase(PhaseRestarting, "", false); err != nil {
		return restore(err, nil)
	}
	newProcess, err := options.Start(plan.CurrentExecutable, plan.OriginalArgs)
	if err != nil {
		return restore(coded(CodeRestartFailed, err), nil)
	}
	if err := setPhase(PhaseVerifying, "", false); err != nil {
		return restore(err, newProcess)
	}
	healthContext, cancelHealth := context.WithTimeout(ctx, time.Duration(plan.HealthWaitMillis)*time.Millisecond)
	err = probeUntilHealthy(healthContext, plan.HealthURL, options.Probe)
	cancelHealth()
	if err != nil {
		return restore(coded(CodeHealthCheckFailed, err), newProcess)
	}
	_ = newProcess.Release()
	state.CurrentVersion = plan.TargetVersion
	if err := setPhase(PhaseSucceeded, "", true); err != nil {
		return err
	}
	pruneBackups(filepath.Dir(plan.Backup), 3)
	return nil
}

func samePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

func probeUntilHealthy(ctx context.Context, healthURL string, probe func(context.Context, string) error) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := probe(ctx, healthURL); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func pruneBackups(directory string, keep int) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	type backup struct {
		path string
		time time.Time
	}
	backups := make([]backup, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".bak" {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			backups = append(backups, backup{path: filepath.Join(directory, entry.Name()), time: info.ModTime()})
		}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].time.After(backups[j].time) })
	if keep < 0 || len(backups) <= keep {
		return
	}
	for _, item := range backups[keep:] {
		_ = os.Remove(item.path)
	}
}
