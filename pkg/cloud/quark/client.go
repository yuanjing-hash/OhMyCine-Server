package quark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type Client struct {
	config            cloud.Config
	api               *apiClient
	mu                sync.RWMutex
	credentials       Credentials
	observed          map[string]cloud.Item
	directoryLocators map[string]directoryLocator
	mediaTransport    http.RoundTripper
	restrictedMu      sync.Mutex
	restrictedLoader  func(context.Context) (json.RawMessage, error)
}

var renewals sync.Map

func New(config cloud.Config) (cloud.Driver, error) { return NewClient(config) }
func NewClient(config cloud.Config) (*Client, error) {
	c, e := ParseCredentials(config.Credentials)
	if e != nil {
		return nil, e
	}
	return &Client{config: config, api: newAPI(config), credentials: c, observed: map[string]cloud.Item{}, mediaTransport: newPublicTransport()}, nil
}
func (d *Client) Provider() string { return cloud.ProviderQuark }
func (d *Client) Credentials() Credentials {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return cloneCredentials(d.credentials)
}
func (d *Client) Capabilities() cloud.Capabilities {
	c := d.Credentials()
	tv := c.TV != nil && c.TV.IdentityVerified
	management := false
	for _, p := range c.Proofs {
		if c.HasProof(p.TVRoot, p.AgentRoot) && d.config.Operations != nil {
			management = true
			break
		}
	}
	return cloud.Capabilities{NetworkDrive: true, DirectoryList: tv, TemporaryDirectURL: tv, SignedProxy: tv, ShareReceive: management, FileUpload: management, CreateDirectory: management, Move: management, Rename: management}
}
func (d *Client) authorization(ctx context.Context, route Route) (Authorization, error) {
	if err := d.loadRestrictedCredentials(ctx); err != nil {
		return Authorization{}, err
	}
	c := d.Credentials()
	a := c.Route(route)
	if a != nil && !a.IdentityVerified && scalar(a.AccessToken, 32768) && d.config.PersistCredentials != nil {
		// A successful rotation is checkpointed before its identity request.
		// Retry that read-only confirmation, never rotate the consumed token again.
		lock := d.credentialLock()
		lock.Lock()
		defer lock.Unlock()
		var err error
		c, err = d.currentCredentials(ctx)
		if err != nil {
			return Authorization{}, err
		}
		a = c.Route(route)
		if a != nil && !a.IdentityVerified && scalar(a.AccessToken, 32768) {
			c, err = (&Authenticator{api: d.api}).confirmLocked(ctx, route, c, d.config.PersistCredentials)
		}
		d.mu.Lock()
		d.credentials = c
		d.mu.Unlock()
		if err != nil {
			return Authorization{}, err
		}
		a = c.Route(route)
	}
	if a == nil || !a.IdentityVerified || !scalar(a.AccessToken, 32768) {
		return Authorization{}, failure(CodeAuth)
	}
	return *a, nil
}

// Both routes share one persisted bundle revision. Serializing only by route
// could consume two refresh tokens while one checkpoint loses the CAS race.
func (d *Client) credentialLock() *sync.Mutex {
	return credentialLock(d.config.ConnectionID)
}

func credentialLock(connectionID uint) *sync.Mutex {
	key := fmt.Sprintf("%d", connectionID)
	v, _ := renewals.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// WithCredentialUpdate serializes a configuration write with exchanges,
// rotations and proof persistence for this connection. The callback must not
// invoke an authorization or provider method that acquires the same lock.
func WithCredentialUpdate(ctx context.Context, connectionID uint, update func() error) error {
	lock := credentialLock(connectionID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return update()
}
func (d *Client) currentCredentials(ctx context.Context) (Credentials, error) {
	c := d.Credentials()
	if d.config.LoadCredentials != nil {
		raw, e := d.config.LoadCredentials(ctx)
		if e != nil {
			return c, e
		}
		c, e = ParseCredentials(raw)
		if e != nil {
			return c, e
		}
	}
	return c, nil
}

// ResumeAuthorizations restores only already-saved exchange/refresh checkpoints.
// A route that remains unavailable does not disable another verified route.
func (d *Client) ResumeAuthorizations(ctx context.Context) error {
	var first error
	for _, route := range []Route{RouteTV, RouteAgent} {
		a := d.Credentials().Route(route)
		if a == nil || a.IdentityVerified || !scalar(a.AccessToken, 32768) {
			continue
		}
		if _, err := d.authorization(ctx, route); err != nil && first == nil {
			first = err
		}
	}
	return first
}
func (d *Client) refresh(ctx context.Context, route Route, generation uint64) error {
	lock := d.credentialLock()
	lock.Lock()
	defer lock.Unlock()
	c, e := d.currentCredentials(ctx)
	if e != nil {
		return e
	}
	old := c.Route(route)
	if old != nil && old.Generation != generation && old.IdentityVerified {
		d.mu.Lock()
		d.credentials = c
		d.mu.Unlock()
		return nil
	}
	var next Credentials
	if old != nil && !old.IdentityVerified && scalar(old.AccessToken, 32768) {
		next, e = (&Authenticator{api: d.api}).confirmLocked(ctx, route, c, d.config.PersistCredentials)
	} else {
		next, e = (&Authenticator{api: d.api}).refreshLocked(ctx, route, c, d.config.PersistCredentials)
	}
	d.mu.Lock()
	d.credentials = next
	d.mu.Unlock()
	return e
}
func (d *Client) tvRead(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return nil, e
	}
	raw, e := d.api.tv(ctx, a, path, q)
	if isAuth(e) && d.config.PersistCredentials != nil {
		if e = d.refresh(ctx, RouteTV, a.Generation); e != nil {
			return nil, e
		}
		a, e = d.authorization(ctx, RouteTV)
		if e != nil {
			return nil, e
		}
		return d.api.tv(ctx, a, path, q)
	}
	return raw, e
}
func (d *Client) agentRead(ctx context.Context, method, path string, q url.Values, body any) (json.RawMessage, error) {
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return nil, e
	}
	raw, e := d.api.agent(ctx, a, method, path, q, body)
	if isAuth(e) && d.config.PersistCredentials != nil {
		if e = d.refresh(ctx, RouteAgent, a.Generation); e != nil {
			return nil, e
		}
		a, e = d.authorization(ctx, RouteAgent)
		if e != nil {
			return nil, e
		}
		return d.api.agent(ctx, a, method, path, q, body)
	}
	return raw, e
}
func (d *Client) Probe(ctx context.Context) (cloud.Account, error) {
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.Account{}, e
	}
	raw, e := d.tvRead(ctx, "/user", url.Values{"method": {"user_info"}})
	if e != nil {
		return cloud.Account{}, e
	}
	data, e := unwrap(raw)
	if e != nil {
		return cloud.Account{}, e
	}
	var user struct {
		ID   numericID `json:"user_id"`
		Name string    `json:"nickname"`
	}
	if json.Unmarshal(data, &user) != nil || string(user.ID) != a.AccountID {
		return cloud.Account{}, failure(CodeIdentity)
	}
	return cloud.Account{ID: QualifyID(RouteTV, a.AccountID, a.AccountID), Name: user.Name}, nil
}
func (d *Client) List(ctx context.Context, parent string, request cloud.PageRequest) (cloud.Page, error) {
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.Page{}, e
	}
	p, e := parseID(parent, RouteTV, a.AccountID, true)
	if e != nil {
		return cloud.Page{}, e
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	if request.Offset < 0 || request.Offset%limit != 0 {
		return cloud.Page{}, failure(CodeInvalid)
	}
	raw, e := d.tvRead(ctx, "/file", url.Values{"method": {"list"}, "parent_fid": {p}, "order_by": {"3"}, "desc": {"1"}, "category": {""}, "source": {""}, "ex_source": {""}, "list_all": {"0"}, "page_size": {strconv.FormatInt(limit, 10)}, "page_index": {strconv.FormatInt(request.Offset/limit, 10)}})
	if e != nil {
		return cloud.Page{}, e
	}
	data, e := unwrap(raw)
	if e != nil {
		return cloud.Page{}, e
	}
	var page struct {
		Total *int64          `json:"total_count"`
		Files json.RawMessage `json:"files"`
	}
	if json.Unmarshal(data, &page) != nil || page.Total == nil || *page.Total < 0 || len(page.Files) == 0 || page.Files[0] != '[' {
		return cloud.Page{}, failure(CodeInvalid)
	}
	var files []struct {
		ID       string `json:"fid"`
		Parent   string `json:"parent_fid"`
		Name     string `json:"filename"`
		Size     *int64 `json:"size"`
		Dir      *int   `json:"isdir"`
		Modified *int64 `json:"updated_at"`
		Created  *int64 `json:"created_at"`
		Version  string `json:"version"`
	}
	if json.Unmarshal(page.Files, &files) != nil || int64(len(files)) > limit || request.Offset+int64(len(files)) > *page.Total || len(files) == 0 && request.Offset < *page.Total {
		return cloud.Page{}, failure(CodeIncomplete)
	}
	result := cloud.Page{Offset: request.Offset, Total: page.Total, HasMore: request.Offset+int64(len(files)) < *page.Total, Items: []cloud.Item{}}
	seen := map[string]bool{}
	for _, f := range files {
		if !scalar(f.ID, 4096) || !filename(f.Name) || f.Size == nil || *f.Size < 0 || f.Dir == nil || *f.Dir != 0 && *f.Dir != 1 || f.Parent != "" && f.Parent != p || seen[f.ID] {
			return cloud.Page{}, failure(CodeInvalid)
		}
		seen[f.ID] = true
		item := cloud.Item{ID: QualifyID(RouteTV, a.AccountID, f.ID), ParentID: QualifyID(RouteTV, a.AccountID, p), Name: f.Name, IsDir: *f.Dir == 1, Size: *f.Size, Revision: f.Version}
		if f.Modified != nil {
			item.ModifiedAt = providerTime(*f.Modified)
		}
		if f.Created != nil {
			item.CreatedAt = providerTime(*f.Created)
		}
		result.Items = append(result.Items, item)
	}
	d.mu.Lock()
	for _, i := range result.Items {
		d.observed[i.ID] = i
	}
	d.mu.Unlock()
	return result, nil
}
func providerTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n > 100000000000 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}
func (d *Client) Stat(ctx context.Context, id string) (cloud.Item, error) {
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.Item{}, e
	}
	rawID, e := parseID(id, RouteTV, a.AccountID, true)
	if e != nil {
		return cloud.Item{}, e
	}
	if rawID == "0" {
		return cloud.Item{ID: QualifyID(RouteTV, a.AccountID, "0"), Name: "/", IsDir: true}, nil
	}
	// Proven roots survive a new driver instance. Resolve their saved path
	// independently rather than attempting a file-download call for a folder.
	for _, proof := range d.Credentials().Proofs {
		if proof.TVRoot == id && proof.TVAccount == a.AccountID {
			root, e := d.resolveTV(ctx, proof.Path)
			if e != nil {
				return cloud.Item{}, e
			}
			if root.ID != id || !root.IsDir {
				return cloud.Item{}, failure(CodeIdentity)
			}
			return root, nil
		}
	}
	d.mu.RLock()
	item, known := d.observed[id]
	d.mu.RUnlock()
	if known {
		for off := int64(0); off < 10000; off += 100 {
			p, e := d.List(ctx, item.ParentID, cloud.PageRequest{Offset: off, Limit: 100})
			if e != nil {
				return cloud.Item{}, e
			}
			for _, f := range p.Items {
				if f.ID == id {
					return f, nil
				}
			}
			if !p.HasMore {
				return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
			}
		}
		return cloud.Item{}, failure(CodeIncomplete)
	}
	// TV has no independently verified stat endpoint. Original download metadata
	// can establish a regular file, never a directory or parent relationship.
	out, _, e := d.original(ctx, rawID)
	return out, e
}
func (d *Client) original(ctx context.Context, id string) (cloud.Item, cloud.TemporaryURL, error) {
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.Item{}, cloud.TemporaryURL{}, e
	}
	raw, e := d.tvRead(ctx, "/file", url.Values{"method": {"download"}, "group_by": {"source"}, "fid": {id}, "resolution": {"low,normal,high,super,2k,4k"}, "support": {"dolby_vision"}})
	if e != nil {
		return cloud.Item{}, cloud.TemporaryURL{}, e
	}
	data, e := unwrap(raw)
	if e != nil {
		return cloud.Item{}, cloud.TemporaryURL{}, e
	}
	var f struct {
		ID      string `json:"fid"`
		Name    string `json:"file_name"`
		Size    *int64 `json:"size"`
		URL     string `json:"download_url"`
		Expires *int64 `json:"expires_at"`
	}
	if json.Unmarshal(data, &f) != nil || f.ID != id || !filename(f.Name) || f.Size == nil || *f.Size < 0 {
		return cloud.Item{}, cloud.TemporaryURL{}, failure(CodeInvalid)
	}
	u, e := directURL(f.URL)
	if e != nil {
		return cloud.Item{}, cloud.TemporaryURL{}, e
	}
	expiry := time.Time{}
	if f.Expires != nil {
		expiry = providerTime(*f.Expires)
	}
	for _, key := range []string{"Expires", "expires", "x-oss-expires", "e"} {
		if n, e := strconv.ParseInt(u.Query().Get(key), 10, 64); e == nil && n > 1000000000 {
			t := providerTime(n)
			if expiry.IsZero() || t.Before(expiry) {
				expiry = t
			}
		}
	}
	if !expiry.IsZero() && !expiry.After(d.api.clock()) {
		return cloud.Item{}, cloud.TemporaryURL{}, failure(CodeAuth)
	}
	// Unknown expiry intentionally disables reusable caching in the integration.
	item := cloud.Item{ID: QualifyID(RouteTV, a.AccountID, id), Name: f.Name, Size: *f.Size}
	return item, cloud.TemporaryURL{URL: f.URL, ExpiresAt: expiry}, nil
}
func directURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || !scalar(raw, 32768) || strings.Contains(raw, "\\") || u.Scheme != "https" && u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return nil, failure(CodeInvalid)
	}
	return u, nil
}
func (d *Client) DirectURL(ctx context.Context, r cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.TemporaryURL{}, e
	}
	id, e := parseID(r.FileID, RouteTV, a.AccountID, false)
	if e != nil {
		return cloud.TemporaryURL{}, e
	}
	_, out, e := d.original(ctx, id)
	return out, e
}
func (d *Client) OpenRead(ctx context.Context, r cloud.ReadRequest) (cloud.ReadResult, error) {
	if r.Offset < 0 {
		return cloud.ReadResult{}, failure(CodeInvalid)
	}
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.ReadResult{}, e
	}
	id, e := parseID(r.FileID, RouteTV, a.AccountID, false)
	if e != nil {
		return cloud.ReadResult{}, e
	}
	item, link, e := d.original(ctx, id)
	if e != nil {
		return cloud.ReadResult{}, e
	}
	body, e := newOriginalRangeReader(ctx, d.mediaTransport, link.URL, item.Size, r.Offset, d.loadRestrictedCredentials)
	if e != nil {
		return cloud.ReadResult{}, e
	}
	return cloud.ReadResult{Body: body, OffsetAccepted: true, TotalSize: &item.Size}, nil
}

var _ cloud.ReadDriver = (*Client)(nil)
