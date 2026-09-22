package pan115

import (
	"errors"
	"net/url"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// NormalizeShareLink accepts the same share contract as InspectShare, merging
// a separately supplied extraction code without retaining unrelated URL fields.
func NormalizeShareLink(raw, password string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return "", "", cloud.Error(cloud.CodeShareInvalid, false, errors.New("share link is invalid"))
	}
	if password != "" {
		q := u.Query()
		q.Set("password", password)
		u.RawQuery = q.Encode()
	}
	code, secret, err := parseShareLink(u.String())
	if err != nil {
		return "", "", err
	}
	return "https://115.com/s/" + url.PathEscape(code) + "?" + url.Values{"password": {secret}}.Encode(), code, nil
}
