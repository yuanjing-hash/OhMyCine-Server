package pan115

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxCookieLength = 16 << 10

type Cookie struct {
	UID  string
	CID  string
	SEID string
	KID  string
}

// CredentialResourceAccount is only a credential-bound mutual-exclusion
// namespace. It is not a successful login/probe or cross-interface identity.
func CredentialResourceAccount(raw string) (string, error) {
	parsed, err := ParseCookie(raw)
	if err != nil {
		return "", err
	}
	return credentialResourceAccount(parsed)
}
func credentialResourceAccount(cookie Cookie) (string, error) {
	value, suffix, hasSuffix := strings.Cut(cookie.UID, "_")
	if value == "" || hasSuffix && suffix == "" {
		return "", errors.New("115 credential account is invalid")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return "", errors.New("115 credential account is invalid")
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return "", errors.New("115 credential account is invalid")
	}
	return strconv.FormatInt(id, 10), nil
}

func ParseCookie(raw string) (Cookie, error) {
	if len(raw) == 0 || len(raw) > maxCookieLength || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\x00\r\n") {
		return Cookie{}, errors.New("115 cookie is invalid")
	}
	values := map[string]string{}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return Cookie{}, errors.New("115 cookie pair is invalid")
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		if key != "UID" && key != "CID" && key != "SEID" && key != "KID" {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n;") {
			return Cookie{}, errors.New("115 cookie value is invalid")
		}
		if _, duplicate := values[key]; duplicate {
			return Cookie{}, errors.New("115 cookie field is duplicated")
		}
		values[key] = value
	}
	if values["UID"] == "" || values["CID"] == "" || values["SEID"] == "" {
		return Cookie{}, errors.New("115 cookie is missing required fields")
	}
	return Cookie{UID: values["UID"], CID: values["CID"], SEID: values["SEID"], KID: values["KID"]}, nil
}

func (c Cookie) String() string {
	parts := []string{"UID=" + c.UID, "CID=" + c.CID, "SEID=" + c.SEID}
	if c.KID != "" {
		parts = append(parts, "KID="+c.KID)
	}
	return strings.Join(parts, "; ")
}
