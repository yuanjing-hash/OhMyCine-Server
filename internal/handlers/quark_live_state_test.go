//go:build quark_live

package handlers_test

// This bridge is only for the explicitly launched acceptance helper. It owns
// the original diagnostic store's runtime lock; the disposable Server receives
// an access-only snapshot, never a fork of the rotating refresh credential.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const liveTVPurpose = "quark-original-302-probe:tv-session:v1"
const liveTVSessionName = "tv-session.enc.json"
const liveTVCheckpointName = "tv-token-checkpoint.enc.json"
const liveTVAttemptName = "tv-manual-refresh-attempt.enc.json"

type liveTVSession struct {
	Version             int       `json:"version"`
	ProfileFingerprint  string    `json:"profile_fingerprint"`
	DeviceID            string    `json:"device_id"`
	AccessToken         string    `json:"access_token,omitempty"`
	RefreshToken        string    `json:"refresh_token,omitempty"`
	UserID              string    `json:"user_id,omitempty"`
	Generation          uint64    `json:"generation"`
	BrokerAuthorization bool      `json:"broker_authorization"`
	BrokerRefresh       bool      `json:"broker_refresh"`
	IdentityVerified    bool      `json:"identity_verified"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type liveTVAttempt struct {
	Version       int    `json:"version"`
	Generation    uint64 `json:"generation"`
	SessionSHA256 string `json:"session_sha256"`
	Failure       string `json:"failure,omitempty"`
}

type liveTVAuthenticator interface {
	Verify(context.Context, quark.Route, quark.Credentials) error
	Refresh(context.Context, quark.Route, quark.Credentials, quark.PersistFunc) (quark.Credentials, error)
	Confirm(context.Context, quark.Route, quark.Credentials, quark.PersistFunc) (quark.Credentials, error)
}

type liveTVStore struct {
	dir        string
	lock       *database.ExclusiveRuntime
	cipher     *credential.Store
	key        []byte
	saved      []byte
	session    liveTVSession
	checkpoint *liveTVSession
	attempt    *liveTVAttempt
	write      func(string, []byte) error
}

// Fixed errors deliberately exclude filesystem paths, account IDs, upstream
// response text, token values and request URLs.
func liveTVError(code string) error { return fmt.Errorf("%s", code) }

func liveTVMessage(err error) string {
	switch err.Error() {
	case "tv_state_locked":
		return "另一个夸克授权或验收窗口正在使用同一会话；请关闭该窗口后重试。"
	case "tv_refresh_denied", "tv_refresh_unavailable":
		return "TV 续期凭据不可用或已被拒绝；请重新运行夸克 TV 扫码授权，再运行本命令。"
	case "tv_refresh_uncertain":
		return "上次 TV 续期未拿到可确认结果，已停止重复提交；请保留授权目录供检查，不要反复重试续期。"
	case "tv_identity_mismatch":
		return "TV 返回的账号与原授权不一致，未替换原登录；已保留加密检查点供检查。"
	case "tv_checkpoint_expired":
		return "已保存的新 TV 会话无法通过身份确认，未再次消费刷新令牌；请重新扫码授权。"
	case "tv_identity_unavailable":
		return "TV 身份检查暂不可用；已保存的会话保留，请稍后重新运行本命令。"
	case "tv_state_changed":
		return "授权文件在本次检查期间发生变化，已停止，避免覆盖其他授权。"
	default:
		return "无法安全读取或保存 TV 授权；原密钥和目录权限未重置，请保留授权目录供检查。"
	}
}

func liveTVRead(path string, limit int64, optional bool) ([]byte, error) {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || storagefs.IsReparsePoint(current, info) {
			return nil, liveTVError("tv_state_invalid")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	info, err := os.Lstat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || storagefs.IsReparsePoint(path, info) || info.Size() > limit {
		return nil, liveTVError("tv_state_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, liveTVError("tv_state_invalid")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, liveTVError("tv_state_changed")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, liveTVError("tv_state_invalid")
	}
	return raw, nil
}

func liveTVDecode(raw []byte, output any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(output) != nil || d.Decode(new(any)) != io.EOF {
		return liveTVError("tv_state_invalid")
	}
	return nil
}

func liveTVScalar(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validLiveTVSession(s liveTVSession) bool {
	device, deviceErr := hex.DecodeString(s.DeviceID)
	profile, profileErr := hex.DecodeString(s.ProfileFingerprint)
	return s.Version == 1 && deviceErr == nil && len(device) == 16 && profileErr == nil && len(profile) == 32 &&
		s.BrokerAuthorization && s.Generation > 0 && s.Generation < ^uint64(0) && liveTVScalar(s.UserID, 4096) &&
		liveTVScalar(s.AccessToken, 32768) && (s.RefreshToken == "" || liveTVScalar(s.RefreshToken, 32768))
}

func openLiveTVStore(dir string) (_ *liveTVStore, returnedError error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, liveTVError("tv_state_invalid")
	}
	// Validate the existing directory/file before acquiring the shared runtime
	// lock. Never create a missing credential directory or generate a new key.
	if _, err = liveTVRead(filepath.Join(abs, liveTVSessionName), 1<<20, false); err != nil {
		return nil, err
	}
	s := &liveTVStore{dir: abs}
	s.lock, err = database.AcquireExclusiveRuntime(filepath.Join(abs, liveTVSessionName))
	if err != nil {
		return nil, liveTVError("tv_state_locked")
	}
	defer func() {
		if returnedError != nil {
			_ = s.lock.Close()
		}
	}()
	s.key, err = liveTVRead(filepath.Join(abs, "tv-credentials.key"), 1024, false)
	if err != nil {
		return nil, err
	}
	s.cipher, err = credential.Open("", string(s.key))
	if err != nil {
		return nil, liveTVError("tv_state_invalid")
	}
	s.saved, err = liveTVRead(filepath.Join(abs, liveTVSessionName), 1<<20, false)
	if err != nil || s.decrypt(s.saved, liveTVPurpose, &s.session) != nil || !validLiveTVSession(s.session) || !s.session.IdentityVerified {
		return nil, liveTVError("tv_state_invalid")
	}
	checkpoint, err := liveTVRead(filepath.Join(abs, liveTVCheckpointName), 1<<20, true)
	if err != nil {
		return nil, err
	}
	if len(checkpoint) > 0 {
		s.checkpoint = new(liveTVSession)
		if s.decrypt(checkpoint, liveTVPurpose+":pending:v1", s.checkpoint) != nil || !validLiveTVSession(*s.checkpoint) || !liveTVScalar(s.checkpoint.RefreshToken, 32768) {
			return nil, liveTVError("tv_state_invalid")
		}
	}
	attempt, err := liveTVRead(filepath.Join(abs, liveTVAttemptName), 16384, true)
	if err != nil {
		return nil, err
	}
	if len(attempt) > 0 {
		s.attempt = new(liveTVAttempt)
		if s.decrypt(attempt, liveTVPurpose+":manual-refresh-attempt:v1", s.attempt) != nil || s.attempt.Version != 1 || s.attempt.Generation == 0 || len(s.attempt.SessionSHA256) != 64 {
			return nil, liveTVError("tv_state_invalid")
		}
	}
	s.write = s.atomicWrite
	return s, nil
}

func (s *liveTVStore) decrypt(raw []byte, purpose string, output any) error {
	var envelope struct {
		Version    int    `json:"version"`
		Ciphertext string `json:"ciphertext"`
	}
	if liveTVDecode(raw, &envelope) != nil || envelope.Version != 1 {
		return liveTVError("tv_state_invalid")
	}
	plain, err := s.cipher.Decrypt(purpose, envelope.Ciphertext)
	if err != nil {
		return liveTVError("tv_state_invalid")
	}
	return liveTVDecode([]byte(plain), output)
}

func (s *liveTVStore) unchanged() error {
	key, keyErr := liveTVRead(filepath.Join(s.dir, "tv-credentials.key"), 1024, false)
	main, mainErr := liveTVRead(filepath.Join(s.dir, liveTVSessionName), 1<<20, false)
	if keyErr != nil || mainErr != nil || !bytes.Equal(key, s.key) || !bytes.Equal(main, s.saved) {
		return liveTVError("tv_state_changed")
	}
	return nil
}

func (s *liveTVStore) save(name, purpose string, value any) ([]byte, error) {
	if err := s.unchanged(); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, liveTVError("tv_state_save_failed")
	}
	ciphertext, err := s.cipher.Encrypt(purpose, string(plain))
	if err != nil {
		return nil, liveTVError("tv_state_save_failed")
	}
	raw, _ := json.Marshal(struct {
		Version    int    `json:"version"`
		Ciphertext string `json:"ciphertext"`
	}{1, ciphertext})
	if err = s.write(name, raw); err != nil {
		return nil, liveTVError("tv_state_save_failed")
	}
	return raw, nil
}

func (s *liveTVStore) atomicWrite(name string, raw []byte) error {
	if name != liveTVSessionName && name != liveTVCheckpointName && name != liveTVAttemptName {
		return liveTVError("tv_state_save_failed")
	}
	target := filepath.Join(s.dir, name)
	if _, err := liveTVRead(target, 1<<20, true); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".tv-manual-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	// Apply the existing main file's security to the temporary encrypted file;
	// do not change the source file or directory permissions.
	if err = liveTVCopySecurity(filepath.Join(s.dir, liveTVSessionName), f.Name()); err != nil {
		return err
	}
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Replace only this helper's encrypted files, under the shared state lock.
	// The existing private parent ACL is inherited; it is never reset here.
	return os.Rename(f.Name(), target)
}

func (s liveTVSession) bundle() quark.Credentials {
	c := quark.NewCredentials()
	c.Revision = 1
	c.TV = &quark.Authorization{AccessToken: s.AccessToken, RefreshToken: s.RefreshToken, DeviceID: s.DeviceID, AccountID: s.UserID, Generation: s.Generation, IdentityVerified: s.IdentityVerified}
	return c
}

func sameLiveTVIdentity(a, b liveTVSession) bool {
	return a.ProfileFingerprint == b.ProfileFingerprint && a.DeviceID == b.DeviceID && a.UserID == b.UserID && a.BrokerAuthorization == b.BrokerAuthorization
}

func sameLiveTVPair(a, b liveTVSession) bool {
	return sameLiveTVIdentity(a, b) && a.Generation == b.Generation && a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
}

func (s *liveTVStore) cleanup() error {
	for _, name := range []string{liveTVCheckpointName, liveTVAttemptName} {
		if _, err := liveTVRead(filepath.Join(s.dir, name), 1<<20, true); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return liveTVError("tv_state_save_failed")
		}
	}
	s.checkpoint, s.attempt = nil, nil
	return nil
}

func (s *liveTVStore) persist(previous *quark.Credentials) quark.PersistFunc {
	return func(ctx context.Context, revision uint64, raw json.RawMessage) error {
		if ctx.Err() != nil || previous.Revision != revision {
			return liveTVError("tv_state_changed")
		}
		next, err := quark.ParseCredentials(raw)
		if err != nil || next.Revision != revision+1 || next.TV == nil || next.Agent != nil || next.Web != nil || len(next.Proofs) != 0 {
			return liveTVError("tv_state_invalid")
		}
		value := s.session
		value.AccessToken, value.RefreshToken = next.TV.AccessToken, next.TV.RefreshToken
		value.DeviceID, value.UserID = next.TV.DeviceID, next.TV.AccountID
		value.Generation, value.IdentityVerified = next.TV.Generation, next.TV.IdentityVerified
		value.UpdatedAt = time.Now().UTC()
		if !validLiveTVSession(value) || !sameLiveTVIdentity(s.session, value) || value.Generation != s.session.Generation+1 {
			return liveTVError("tv_identity_mismatch")
		}
		if !value.IdentityVerified {
			if s.checkpoint != nil {
				return liveTVError("tv_state_changed")
			}
			value.BrokerRefresh = false
			if _, err = s.save(liveTVCheckpointName, liveTVPurpose+":pending:v1", value); err != nil {
				return err
			}
			s.checkpoint = &value
		} else {
			if s.checkpoint == nil || !sameLiveTVPair(value, *s.checkpoint) {
				return liveTVError("tv_state_changed")
			}
			value.BrokerRefresh = true
			var saved []byte
			if saved, err = s.save(liveTVSessionName, liveTVPurpose, value); err != nil {
				return err
			}
			s.saved, s.session = saved, value
			if err = s.cleanup(); err != nil {
				return err
			}
		}
		*previous = next
		return nil
	}
}

func (s *liveTVStore) ready(ctx context.Context, auth liveTVAuthenticator) (quark.Credentials, bool, error) {
	if err := s.unchanged(); err != nil {
		return quark.Credentials{}, false, err
	}
	if s.checkpoint != nil {
		pending := *s.checkpoint
		if sameLiveTVPair(pending, s.session) {
			// Promotion was already committed before interrupted cleanup. This
			// pair is not an unverified exchange: retire only its matching stale
			// recovery records, then use the normal verify/renew path below.
			if err := s.cleanup(); err != nil {
				return quark.Credentials{}, false, err
			}
		} else {
			if !sameLiveTVIdentity(pending, s.session) || pending.Generation != s.session.Generation+1 || pending.IdentityVerified {
				return quark.Credentials{}, false, liveTVError("tv_state_invalid")
			}
			c := pending.bundle()
			if _, err := auth.Confirm(ctx, quark.RouteTV, c, s.persist(&c)); err != nil {
				return quark.Credentials{}, false, liveTVAuthError(err, true)
			}
			return s.accessOnly(), true, nil
		}
	}
	c := s.session.bundle()
	err := auth.Verify(ctx, quark.RouteTV, c)
	if err == nil {
		// A successful explicit QR login can supersede an older ambiguous
		// attempt. Retire that obsolete marker only after verifying the newer
		// saved identity; retain a marker tied to this same generation.
		if s.attempt != nil && s.attempt.Generation < s.session.Generation {
			if err := s.cleanup(); err != nil {
				return quark.Credentials{}, false, err
			}
		}
		return s.accessOnly(), false, nil
	}
	if code, _ := cloud.ErrorInfo(err); code != quark.CodeAuth {
		return quark.Credentials{}, false, liveTVAuthError(err, false)
	}
	if s.attempt != nil {
		// Never repeat a potentially consuming request after a lost response.
		// A new explicit QR login changes the generation/pair; the old marker is
		// then obsolete and may be removed without consuming its old token.
		digest := sha256.Sum256(s.saved)
		if s.attempt.Generation == s.session.Generation && s.attempt.SessionSHA256 == hex.EncodeToString(digest[:]) {
			if s.attempt.Failure == "denied" {
				return quark.Credentials{}, false, liveTVError("tv_refresh_denied")
			}
			return quark.Credentials{}, false, liveTVError("tv_refresh_uncertain")
		}
		if s.attempt.Generation >= s.session.Generation {
			return quark.Credentials{}, false, liveTVError("tv_state_changed")
		}
	}
	if s.session.RefreshToken == "" {
		return quark.Credentials{}, false, liveTVError("tv_refresh_unavailable")
	}
	digest := sha256.Sum256(s.saved)
	s.attempt = &liveTVAttempt{Version: 1, Generation: s.session.Generation, SessionSHA256: hex.EncodeToString(digest[:])}
	if _, err = s.save(liveTVAttemptName, liveTVPurpose+":manual-refresh-attempt:v1", s.attempt); err != nil {
		return quark.Credentials{}, false, err
	}
	if _, err = auth.Refresh(ctx, quark.RouteTV, c, s.persist(&c)); err != nil {
		if s.checkpoint != nil {
			return quark.Credentials{}, false, liveTVAuthError(err, true)
		}
		if code, _ := cloud.ErrorInfo(err); code == quark.CodeAuth {
			s.attempt.Failure = "denied"
			_, _ = s.save(liveTVAttemptName, liveTVPurpose+":manual-refresh-attempt:v1", s.attempt)
			return quark.Credentials{}, false, liveTVError("tv_refresh_denied")
		}
		return quark.Credentials{}, false, liveTVError("tv_refresh_uncertain")
	}
	return s.accessOnly(), true, nil
}

func (s *liveTVStore) accessOnly() quark.Credentials {
	c := s.session.bundle()
	c.TV.RefreshToken = ""
	return c
}

func liveTVAuthError(err error, checkpoint bool) error {
	code, _ := cloud.ErrorInfo(err)
	if code == quark.CodeIdentity {
		return liveTVError("tv_identity_mismatch")
	}
	if code == quark.CodeAuth && checkpoint {
		return liveTVError("tv_checkpoint_expired")
	}
	if strings.HasPrefix(err.Error(), "tv_state_") {
		return err
	}
	return liveTVError("tv_identity_unavailable")
}
