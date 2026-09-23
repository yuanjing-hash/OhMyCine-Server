package pan115

import (
	"errors"
	"html"
	"net/url"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// NormalizeShareLink accepts the same share contract as InspectShare, merging
// a separately supplied extraction code without retaining unrelated URL fields.
func NormalizeShareLink(raw, password string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	for i := 0; i < 2; i++ {
		raw = html.UnescapeString(raw)
	}
	password = strings.TrimSpace(html.UnescapeString(password))
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return "", "", cloud.Error(cloud.CodeShareInvalid, false, errors.New("share link is invalid"))
	}
	q, queryErr := url.ParseQuery(u.RawQuery)
	if queryErr != nil {
		return "", "", cloud.Error(cloud.CodeShareInvalid, false, nil)
	}
	if password != "" {
		q.Set("password", password)
		u.RawQuery = q.Encode()
	}
	code, secret, err := parseShareLink(u.String())
	if err != nil {
		return "", "", err
	}
	return "https://115.com/s/" + url.PathEscape(code) + "?" + url.Values{"password": {secret}}.Encode(), code, nil
}
