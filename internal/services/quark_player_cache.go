package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

const (
	quarkPlayerCacheMaximum   = 1024
	quarkPlayerCacheTTL       = 30 * time.Second
	quarkPlayerResolveTimeout = time.Minute
)

// This cache is exclusively for authenticated Player requests. STRM resolution
// retains its own contract. Each hit still reads current local authorization,
// Catalog/root/config and credential fences. Only external, not-yet-observed
// cloud changes can remain unseen for this non-sliding, at-most-30s window.
type quarkPlayerPlaybackCache struct {
	mu     sync.Mutex
	scopes map[string]*quarkPlayerScope
	active int
}

type quarkPlayerScope struct {
	generation  uint64
	entry       *quarkPlayerCacheEntry
	proof       *quarkPlayerMembershipProof
	flights     map[string]*quarkPlayerFlight
	activeLinks int
}

// A successful directory observation can outlive one freshly acquired URL.
// Unknown URL expiry never authorizes URL reuse: this record contains no URL.
type quarkPlayerMembershipProof struct {
	key     string
	item    cloud.Item
	expires time.Time
}

// Only a successful request whose proof expired while acquiring its own link
// can enter this one-time recovery. It cannot join a newer scope generation.
type quarkPlayerProofRecheck struct {
	scope      *quarkPlayerScope
	generation uint64
	key        string
}

type quarkPlayerCacheEntry struct {
	key      string
	redirect ProxyRedirect
	expires  time.Time
}

type quarkPlayerFlight struct {
	done       chan struct{}
	generation uint64
	redirect   ProxyRedirect
	err        error
	cancelled  bool
	proof      *quarkPlayerMembershipProof
}

// The key contains hashes only; neither ciphertext nor URLs are map keys.
// Reading ciphertext for its revision identity does not decrypt or refresh it.
func (s *SignedProxyService) quarkPlayerFence(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr string) (string, error) {
	if source.Storage.Type != models.StorageTypeQuark || !source.Library.Enabled || !source.Storage.Enabled || source.Storage.ConnectionID == nil || *source.Storage.ConnectionID == 0 || source.Entry.ProviderID == "" || s.connections == nil {
		return "", PlayerStreamUnavailableError()
	}
	var connection models.Connection
	if err := s.db.WithContext(ctx).Select("id", "provider", "revision", "account_id", "enabled", "credential_ciphertext").First(&connection, *source.Storage.ConnectionID).Error; err != nil || !connection.Enabled || connection.Provider != cloud.ProviderQuark || connection.CredentialCiphertext == "" {
		return "", PlayerStreamUnavailableError()
	}
	credentialHash := sha256.Sum256([]byte(connection.CredentialCiphertext))
	encoded, _ := json.Marshal([]any{
		actor.User.ID, source.Entry.ID, source.Entry.LibraryID, source.Entry.RelativePath, source.Entry.ProviderID, source.Entry.Size, source.Entry.ModifiedAt.UTC(),
		source.Library.StorageID, source.Library.RelativeRoot, source.Library.ProviderRootID, source.Library.ProfileID, source.Library.ProfileRevision,
		source.Storage.ID, source.Storage.RootPath, source.Storage.Type,
		// A publication can carry a provider content-version change without
		// changing visible size/mtime. Conservatively fence every new head.
		source.Head.Mode, source.Head.Revision, source.Head.SourceEpoch, source.Head.SourceFingerprint, source.Head.ConfigFingerprint,
		connection.ID, connection.Provider, connection.Revision, connection.AccountID, hex.EncodeToString(credentialHash[:]),
		userAgent, playbackClientFingerprint(remoteAddr, userAgent),
	})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func quarkPlayerScopeKey(source playerStreamSource) string {
	return strconv.FormatUint(uint64(uintPointerValue(source.Storage.ConnectionID)), 10) + ":" + strconv.FormatUint(uint64(source.Library.ID), 10) + ":" + strconv.FormatUint(uint64(source.Entry.ID), 10)
}

// Called under the cache mutex. Busy scopes cannot be evicted: their generation
// must remain the authority until every in-flight result has been fenced.
func (c *quarkPlayerPlaybackCache) scope(key string) *quarkPlayerScope {
	if c.scopes == nil {
		c.scopes = make(map[string]*quarkPlayerScope)
	}
	if scope := c.scopes[key]; scope != nil {
		return scope
	}
	if len(c.scopes) >= quarkPlayerCacheMaximum {
		for candidate, scope := range c.scopes {
			if len(scope.flights) == 0 && scope.activeLinks == 0 {
				delete(c.scopes, candidate)
				break
			}
		}
		if len(c.scopes) >= quarkPlayerCacheMaximum {
			return nil
		}
	}
	scope := &quarkPlayerScope{flights: make(map[string]*quarkPlayerFlight)}
	c.scopes[key] = scope
	return scope
}

func (s *SignedProxyService) validateQuarkPlayerFence(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := s.playerStreamSource(ctx, actor, source.Entry.ID)
	if err != nil {
		return err
	}
	key, err := s.quarkPlayerFence(ctx, actor, current, userAgent, remoteAddr)
	if err != nil {
		return err
	}
	if key != expected {
		return PlayerStreamUnavailableError()
	}
	return nil
}

func (s *SignedProxyService) resolveQuarkPlayerEntry(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr string, fresh bool) (PlayerStreamResolution, error) {
	return s.resolveQuarkPlayerEntryAttempt(ctx, actor, source, userAgent, remoteAddr, fresh, nil)
}

func (s *SignedProxyService) resolveQuarkPlayerEntryAttempt(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr string, fresh bool, recheck *quarkPlayerProofRecheck) (PlayerStreamResolution, error) {
	key, err := s.quarkPlayerFence(ctx, actor, source, userAgent, remoteAddr)
	if err != nil {
		return PlayerStreamResolution{}, err
	}
	if recheck != nil {
		if key != recheck.key {
			return PlayerStreamResolution{}, PlayerStreamUnavailableError()
		}
		if err := s.validateQuarkPlayerFence(ctx, actor, source, userAgent, remoteAddr, recheck.key); err != nil {
			return PlayerStreamResolution{}, err
		}
	}
	c := &s.quarkPlayer
	scopeKey := quarkPlayerScopeKey(source)
	// A follower may retry once when the previous leader was cancelled (seek).
	// Provider failures are never retried here or replaced by an older result.
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return PlayerStreamResolution{}, err
		}
		c.mu.Lock()
		scope := c.scope(scopeKey)
		if scope == nil || recheck != nil && (scope != recheck.scope || scope.generation != recheck.generation) {
			c.mu.Unlock()
			return PlayerStreamResolution{}, PlayerStreamUnavailableError()
		}
		if fresh {
			scope.generation++
			scope.entry = nil
			scope.proof = nil
			fresh = false
		}
		generation := scope.generation
		if cached := scope.entry; cached != nil && cached.key == key && s.now().Before(cached.expires) {
			c.mu.Unlock()
			if err := s.validateQuarkPlayerFence(ctx, actor, source, userAgent, remoteAddr, key); err != nil {
				return PlayerStreamResolution{}, err
			}
			c.mu.Lock()
			valid := c.scopes[scopeKey] == scope && scope.generation == generation && scope.entry == cached && s.now().Before(cached.expires)
			c.mu.Unlock()
			if !valid {
				return PlayerStreamResolution{}, PlayerStreamUnavailableError()
			}
			return PlayerStreamResolution{Kind: playerStreamKindRedirect, RedirectURL: cached.redirect.URL}, nil
		}
		if proof := scope.proof; proof != nil && proof.key == key && s.now().Before(proof.expires) {
			c.mu.Unlock()
			return s.resolveQuarkPlayerWithProof(ctx, actor, source, userAgent, remoteAddr, key, scopeKey, scope, generation, proof, recheck == nil)
		}
		flightKey := key + ":" + strconv.FormatUint(generation, 10)
		if flight := scope.flights[flightKey]; flight != nil {
			c.mu.Unlock()
			waitCtx, cancel := context.WithTimeout(ctx, quarkPlayerResolveTimeout)
			select {
			case <-waitCtx.Done():
				cancel()
				return PlayerStreamResolution{}, appError(CodeProxyUpstreamUnavailable, "播放请求已取消或超时", waitCtx.Err())
			case <-flight.done:
				cancel()
			}
			if flight.cancelled && ctx.Err() == nil && attempt == 0 {
				continue
			}
			if flight.err != nil {
				return PlayerStreamResolution{}, flight.err
			}
			if err := s.validateQuarkPlayerFence(ctx, actor, source, userAgent, remoteAddr, key); err != nil {
				return PlayerStreamResolution{}, err
			}
			c.mu.Lock()
			valid := c.scopes[scopeKey] == scope && scope.generation == flight.generation
			c.mu.Unlock()
			if !valid {
				return PlayerStreamResolution{}, PlayerStreamUnavailableError()
			}
			if flight.redirect.URL != "" && !flight.redirect.ExpiresAt.IsZero() && quarkPlayerRedirectUnexpired(flight.redirect, s.now()) {
				return PlayerStreamResolution{Kind: playerStreamKindRedirect, RedirectURL: flight.redirect.URL}, nil
			}
			if flight.proof != nil {
				return s.resolveQuarkPlayerWithProof(ctx, actor, source, userAgent, remoteAddr, key, scopeKey, scope, flight.generation, flight.proof, recheck == nil)
			}
			// No reusable URL and the original observation already aged out.
			// Retry the directory once, never reuse an unknown-expiry URL.
			continue
		}
		if c.active >= quarkPlayerCacheMaximum {
			c.mu.Unlock()
			return PlayerStreamResolution{}, PlayerStreamUnavailableError()
		}
		flight := &quarkPlayerFlight{done: make(chan struct{}), generation: generation}
		scope.flights[flightKey] = flight
		c.active++
		c.mu.Unlock()

		verifiedAt := s.now()
		readCtx, cancel := context.WithTimeout(ctx, quarkPlayerResolveTimeout)
		redirect, item, resolveErr := s.resolveQuarkPlayerFresh(readCtx, actor, source, userAgent, remoteAddr, key)
		cancelled := readCtx.Err() != nil
		if cancelled {
			resolveErr = appError(CodeProxyUpstreamUnavailable, "播放请求已取消或超时", readCtx.Err())
		}
		cancel()
		c.mu.Lock()
		if ctx.Err() != nil {
			resolveErr = appError(CodeProxyUpstreamUnavailable, "播放请求已取消", ctx.Err())
			cancelled = true
		}
		current := scope.generation == generation && c.scopes[scopeKey] == scope
		if !current {
			resolveErr = PlayerStreamUnavailableError()
			cancelled = false
		}
		if resolveErr == nil && !quarkPlayerRedirectUnexpired(redirect, s.now()) {
			resolveErr = PlayerStreamUnavailableError()
		}
		if resolveErr == nil {
			now := s.now()
			expires := verifiedAt.Add(quarkPlayerCacheTTL)
			if !redirect.ExpiresAt.IsZero() && redirect.ExpiresAt.Add(-30*time.Second).Before(expires) {
				expires = redirect.ExpiresAt.Add(-30 * time.Second)
			}
			if expires.After(now) {
				proof := &quarkPlayerMembershipProof{key: key, item: item, expires: expires}
				scope.proof, flight.proof = proof, proof
				if !redirect.ExpiresAt.IsZero() {
					scope.entry = &quarkPlayerCacheEntry{key: key, redirect: redirect, expires: expires}
				}
			}
		} else {
			redirect = ProxyRedirect{}
			if current && !cancelled {
				// A different client key can force cold work while the scope
				// still has another client's proof. Its explicit failure also
				// fences that proof and any late, already-running link lookup.
				scope.generation++
				scope.entry, scope.proof = nil, nil
			}
		}
		// Followers may share a known-expiry result, but an unknown-expiry
		// URL belongs only to the request which actually acquired it.
		if !redirect.ExpiresAt.IsZero() {
			flight.redirect = redirect
		}
		flight.err, flight.cancelled = resolveErr, cancelled
		delete(scope.flights, flightKey)
		c.active--
		close(flight.done)
		c.mu.Unlock()
		if resolveErr != nil {
			return PlayerStreamResolution{}, resolveErr
		}
		return PlayerStreamResolution{Kind: playerStreamKindRedirect, RedirectURL: redirect.URL}, nil
	}
	return PlayerStreamResolution{}, PlayerStreamUnavailableError()
}

func quarkPlayerRedirectUnexpired(redirect ProxyRedirect, now time.Time) bool {
	return redirect.URL != "" && (redirect.ExpiresAt.IsZero() || now.Before(redirect.ExpiresAt))
}

func (s *SignedProxyService) quarkPlayerDriver(source playerStreamSource) (cloud.Driver, error) {
	_, driver, err := s.connections.driver(*source.Storage.ConnectionID)
	if mapped := quarkPlaybackError(models.StorageTypeQuark, err); mapped != nil {
		return nil, mapped
	}
	if err != nil || !driver.Capabilities().TemporaryDirectURL {
		return nil, PlayerStreamUnavailableError()
	}
	return driver, nil
}

func (s *SignedProxyService) resolveQuarkPlayerFresh(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr, key string) (ProxyRedirect, cloud.Item, error) {
	driver, err := s.quarkPlayerDriver(source)
	if err != nil {
		return ProxyRedirect{}, cloud.Item{}, err
	}
	rootID := strings.TrimSpace(source.Library.ProviderRootID)
	if rootID == "" {
		rootID = strings.TrimSpace(source.Storage.RootPath)
	}
	item, err := resolvePlaybackItem(cloud.WithReadClass(ctx, cloud.ReadClassInteractive), driver, models.StorageTypeQuark, rootID, source.Entry.RelativePath, source.Entry.ProviderID)
	if err != nil {
		return ProxyRedirect{}, cloud.Item{}, err
	}
	redirect, err := s.resolveQuarkPlayerLink(ctx, actor, source, userAgent, remoteAddr, key, driver, item)
	return redirect, item, err
}

func (s *SignedProxyService) resolveQuarkPlayerLink(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr, key string, driver cloud.Driver, item cloud.Item) (ProxyRedirect, error) {
	if err := s.validateQuarkPlayerFence(ctx, actor, source, userAgent, remoteAddr, key); err != nil {
		return ProxyRedirect{}, err
	}
	rootID := strings.TrimSpace(source.Library.ProviderRootID)
	if rootID == "" {
		rootID = strings.TrimSpace(source.Storage.RootPath)
	}
	target := signedProxyTarget{LibraryID: source.Library.ID, ConnectionID: *source.Storage.ConnectionID, ProviderItemID: item.ID, StorageType: models.StorageTypeQuark, ProviderRootID: rootID, LibraryEnabled: true, StorageEnabled: true}
	// Deliberately bypass the general five-minute STRM URL cache. The single
	// flight covers the initial membership and link acquisition. A proof-only
	// hit always acquires its own link; a failed forced check cannot fall back.
	redirect, err := s.resolveProvider(ctx, playerEntryProxyIdentity(source.Library.ID, source.Entry.ID, item.ID), target, userAgent, playbackClientFingerprint(remoteAddr, userAgent), &item)
	if err != nil {
		return ProxyRedirect{}, err
	}
	if err := s.validateQuarkPlayerFence(ctx, actor, source, userAgent, remoteAddr, key); err != nil {
		return ProxyRedirect{}, err
	}
	return redirect, nil
}

func (s *SignedProxyService) resolveQuarkPlayerWithProof(ctx context.Context, actor Actor, source playerStreamSource, userAgent, remoteAddr, key, scopeKey string, scope *quarkPlayerScope, generation uint64, proof *quarkPlayerMembershipProof, allowExpiryRecheck bool) (PlayerStreamResolution, error) {
	if err := s.validateQuarkPlayerFence(ctx, actor, source, userAgent, remoteAddr, key); err != nil {
		return PlayerStreamResolution{}, err
	}
	c := &s.quarkPlayer
	c.mu.Lock()
	if c.scopes[scopeKey] != scope || scope.generation != generation || scope.proof != proof || proof.key != key || !s.now().Before(proof.expires) || c.active >= quarkPlayerCacheMaximum {
		c.mu.Unlock()
		return PlayerStreamResolution{}, PlayerStreamUnavailableError()
	}
	// Each request owns one link lookup. It is not an unknown-expiry URL
	// singleflight. Reserve capacity and prevent eviction during this call.
	c.active++
	scope.activeLinks++
	c.mu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, quarkPlayerResolveTimeout)
	defer cancel()
	driver, resolveErr := s.quarkPlayerDriver(source)
	redirect := ProxyRedirect{}
	if resolveErr == nil {
		redirect, resolveErr = s.resolveQuarkPlayerLink(readCtx, actor, source, userAgent, remoteAddr, key, driver, proof.item)
	}
	cancelled := readCtx.Err() != nil
	if cancelled {
		resolveErr = appError(CodeProxyUpstreamUnavailable, "播放请求已取消或超时", readCtx.Err())
	}
	c.mu.Lock()
	c.active--
	scope.activeLinks--
	if ctx.Err() != nil {
		resolveErr = appError(CodeProxyUpstreamUnavailable, "播放请求已取消", ctx.Err())
		cancelled = true
	}
	current := c.scopes[scopeKey] == scope && scope.generation == generation && scope.proof == proof
	if !current {
		c.mu.Unlock()
		return PlayerStreamResolution{}, PlayerStreamUnavailableError()
	}
	if resolveErr != nil {
		if !cancelled {
			// A provider/auth failure invalidates this proof generation. Another
			// already-started request cannot subsequently repopulate success.
			scope.generation++
			scope.entry, scope.proof = nil, nil
		}
		c.mu.Unlock()
		return PlayerStreamResolution{}, resolveErr
	}
	if !quarkPlayerRedirectUnexpired(redirect, s.now()) {
		c.mu.Unlock()
		return PlayerStreamResolution{}, PlayerStreamUnavailableError()
	}
	if !s.now().Before(proof.expires) {
		c.mu.Unlock()
		if !allowExpiryRecheck {
			return PlayerStreamResolution{}, PlayerStreamUnavailableError()
		}
		// Discard this link. Recheck membership and acquire a different fresh
		// link once under the same deadline, key and scope generation. Ordinary
		// provider errors, cancellation and changed local fences never enter it.
		return s.resolveQuarkPlayerEntryAttempt(readCtx, actor, source, userAgent, remoteAddr, false, &quarkPlayerProofRecheck{scope: scope, generation: generation, key: key})
	}
	if !redirect.ExpiresAt.IsZero() {
		expires := redirect.ExpiresAt.Add(-30 * time.Second)
		if expires.After(proof.expires) {
			expires = proof.expires
		}
		if expires.After(s.now()) {
			scope.entry = &quarkPlayerCacheEntry{key: key, redirect: redirect, expires: expires}
		}
	}
	c.mu.Unlock()
	return PlayerStreamResolution{Kind: playerStreamKindRedirect, RedirectURL: redirect.URL}, nil
}
