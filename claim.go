package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Claiming uses only an explicitly supplied cookie export. It never reads a
// browser profile, GitHub CLI credential, keychain, environment token, or API.
type githubClaimer struct {
	http     *http.Client
	expected string
}

func newGitHubClaimer(cookiePath, expectedAccount string) (*githubClaimer, error) {
	if !claimAccountName(expectedAccount) {
		return nil, errors.New("claim requires a valid expected GitHub account name")
	}
	file, err := os.Open(cookiePath)
	if err != nil {
		return nil, errors.New("could not open the supplied cookie file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("cookie export must be a readable regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("cookie file must be private; set its permissions to 0600")
	}
	data, err := io.ReadAll(io.LimitReader(file, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return nil, errors.New("could not read cookie export or it exceeds 2 MiB")
	}
	cookies, err := claimCookies(data, time.Now())
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, errors.New("could not initialize the private GitHub cookie jar")
	}
	origin, _ := url.Parse("https://github.com/")
	jar.SetCookies(origin, cookies)
	return &githubClaimer{
		expected: expectedAccount,
		http:     &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: claimRedirect},
	}, nil
}

func claimAccountName(name string) bool {
	if len(name) < 1 || len(name) > 39 || name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func claimCookies(data []byte, now time.Time) ([]*http.Cookie, error) {
	var cookies []*http.Cookie
	accept := func(cookie *http.Cookie) error {
		domain := strings.ToLower(cookie.Domain)
		if (domain != "github.com" && domain != ".github.com") || !cookie.Secure || (!cookie.Expires.IsZero() && !cookie.Expires.After(now)) {
			return nil
		}
		if cookie.Path == "" {
			cookie.Path = "/"
		}
		if cookie.Valid() != nil {
			return errors.New("cookie export contains an invalid GitHub cookie")
		}
		cookies = append(cookies, cookie)
		return nil
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("cookie export is empty")
	}
	if trimmed[0] == '[' || trimmed[0] == '{' {
		var entries []struct {
			Domain         string  `json:"domain"`
			Name           string  `json:"name"`
			Value          string  `json:"value"`
			Path           string  `json:"path"`
			Secure         bool    `json:"secure"`
			HTTPOnly       bool    `json:"httpOnly"`
			ExpirationDate float64 `json:"expirationDate"`
			Expires        float64 `json:"expires"`
		}
		array := trimmed
		if trimmed[0] == '{' {
			var export struct {
				Cookies json.RawMessage `json:"cookies"`
			}
			if json.Unmarshal(trimmed, &export) != nil || len(export.Cookies) == 0 {
				return nil, errors.New("invalid JSON cookie export; expected an array or an object containing cookies")
			}
			array = export.Cookies
		}
		if json.Unmarshal(array, &entries) != nil {
			return nil, errors.New("invalid JSON cookie export")
		}
		for _, entry := range entries {
			cookie := &http.Cookie{Name: entry.Name, Value: entry.Value, Domain: entry.Domain, Path: entry.Path, Secure: entry.Secure, HttpOnly: entry.HTTPOnly}
			expires := entry.ExpirationDate
			if entry.Expires > expires {
				expires = entry.Expires
			}
			if expires > 0 {
				if expires > 253402300799 {
					return nil, errors.New("invalid cookie expiration")
				}
				cookie.Expires = time.Unix(int64(expires), 0)
			}
			if err := accept(cookie); err != nil {
				return nil, err
			}
		}
	} else {
		for _, line := range strings.Split(string(trimmed), "\n") {
			line = strings.TrimSuffix(line, "\r")
			httpOnly := strings.HasPrefix(line, "#HttpOnly_")
			if httpOnly {
				line = strings.TrimPrefix(line, "#HttpOnly_")
			}
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) != 7 {
				return nil, errors.New("invalid Netscape cookie export")
			}
			cookie := &http.Cookie{Domain: fields[0], Path: fields[2], Secure: fields[3] == "TRUE", Name: fields[5], Value: fields[6], HttpOnly: httpOnly}
			expires, err := strconv.ParseInt(fields[4], 10, 64)
			if err != nil || expires < 0 || expires > 253402300799 {
				return nil, errors.New("invalid Netscape cookie expiration")
			}
			if expires > 0 {
				cookie.Expires = time.Unix(expires, 0)
			}
			if err := accept(cookie); err != nil {
				return nil, err
			}
		}
	}
	if len(cookies) == 0 {
		return nil, errors.New("cookie export contains no unexpired secure github.com cookies")
	}
	return cookies, nil
}

func claimAllowedURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, prefix := range []string{"/login", "/session", "/sudo", "/auth", "/two-factor", "/captcha", "/challenge"} {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return true
}

func claimRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || !claimAllowedURL(req.URL) || req.Method != http.MethodGet {
		return errors.New("GitHub session needs attention; refresh the exported cookies")
	}
	return nil
}

func (g *githubClaimer) settings(ctx context.Context) (string, url.Values, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/settings/admin", nil)
	req.Header.Set("Accept", "text/html")
	client := *g.http
	client.CheckRedirect = claimRedirect
	response, err := client.Do(req)
	if err != nil {
		return "", nil, errors.New("could not read GitHub account settings; the session may need fresh exported cookies")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 || response.StatusCode != http.StatusOK {
		return "", nil, errors.New("GitHub account settings were unavailable; refresh the exported session cookies")
	}
	return claimPage(string(data))
}

func (g *githubClaimer) Claim(ctx context.Context, newUsername string) error {
	if !shortName.MatchString(newUsername) || strings.EqualFold(newUsername, g.expected) {
		return errors.New("claim target must be a different three- or four-character lowercase GitHub username")
	}
	if g.http == nil || g.http.Jar == nil || !claimAccountName(g.expected) {
		return errors.New("claim requires an explicit cookie file and expected account")
	}
	account, fields, err := g.settings(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(account, g.expected) {
		return errors.New("authenticated GitHub account does not match the expected account; no rename submitted")
	}
	if fields == nil {
		return errors.New("GitHub rename form was not found or is ambiguous; no rename submitted")
	}
	fields.Set("login", newUsername)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/account/rename", strings.NewReader(fields.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Origin", "https://github.com")
	req.Header.Set("Referer", "https://github.com/settings/admin")
	client := *g.http
	// Never follow a mutation redirect: 307/308 could repeat the POST.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return errors.New("rename outcome is unknown; check your GitHub account before retrying; the POST was not retried")
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	response.Body.Close()
	if response.StatusCode == http.StatusFound || response.StatusCode == http.StatusSeeOther {
		location, err := url.Parse(response.Header.Get("Location"))
		if err != nil || response.Header.Get("Location") == "" || !claimAllowedURL(req.URL.ResolveReference(location)) {
			return errors.New("rename outcome was not confirmed; check the GitHub account and refresh session cookies before retrying")
		}
	} else if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("rename outcome was not confirmed; check your GitHub account before retrying; no second POST was sent")
	}
	account, _, err = g.settings(ctx)
	if err != nil || !strings.EqualFold(account, newUsername) {
		return errors.New("rename outcome was not confirmed; check your GitHub account before retrying; no second POST was sent")
	}
	return nil
}

type claimTag struct {
	name    string
	closing bool
	attrs   map[string]string
}

// This intentionally limited scanner accepts only unambiguous HTML tags. It
// skips comments/raw-text elements and rejects malformed or duplicate attrs.
func claimTags(page string) ([]claimTag, error) {
	var tags []claimTag
	for pos := 0; pos < len(page); {
		relative := strings.IndexByte(page[pos:], '<')
		if relative < 0 {
			break
		}
		pos += relative
		if strings.HasPrefix(page[pos:], "<!--") {
			end := strings.Index(page[pos+4:], "-->")
			if end < 0 {
				return nil, errors.New("malformed GitHub settings markup")
			}
			pos += 4 + end + 3
			continue
		}
		end, quote := pos+1, byte(0)
		for ; end < len(page); end++ {
			c := page[end]
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '\'' || c == '"' {
				quote = c
			} else if c == '>' {
				break
			}
		}
		if end == len(page) {
			return nil, errors.New("malformed GitHub settings markup")
		}
		text := strings.TrimSpace(page[pos+1 : end])
		pos = end + 1
		if text == "" || text[0] == '!' || text[0] == '?' {
			continue
		}
		tag := claimTag{attrs: make(map[string]string)}
		if text[0] == '/' {
			tag.closing = true
			text = strings.TrimSpace(text[1:])
		}
		i := 0
		for i < len(text) && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' && text[i] != '/' {
			i++
		}
		tag.name = strings.ToLower(text[:i])
		text = strings.TrimSpace(text[i:])
		for !tag.closing && text != "" && text != "/" {
			i = 0
			for i < len(text) && text[i] != '=' && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' && text[i] != '/' {
				i++
			}
			if i == 0 {
				return nil, errors.New("ambiguous GitHub settings markup")
			}
			key := strings.ToLower(text[:i])
			text = strings.TrimSpace(text[i:])
			value := ""
			if strings.HasPrefix(text, "=") {
				text = strings.TrimSpace(text[1:])
				if text == "" {
					return nil, errors.New("malformed GitHub settings markup")
				}
				if text[0] == '\'' || text[0] == '"' {
					quote := text[0]
					end := strings.IndexByte(text[1:], quote)
					if end < 0 {
						return nil, errors.New("malformed GitHub settings markup")
					}
					value, text = text[1:1+end], strings.TrimSpace(text[2+end:])
				} else {
					i = 0
					for i < len(text) && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' {
						i++
					}
					value, text = text[:i], strings.TrimSpace(text[i:])
				}
			}
			if _, duplicate := tag.attrs[key]; duplicate {
				return nil, errors.New("ambiguous GitHub settings attributes")
			}
			tag.attrs[key] = html.UnescapeString(value)
		}
		tags = append(tags, tag)
		if !tag.closing && (tag.name == "script" || tag.name == "style" || tag.name == "textarea" || tag.name == "title" || tag.name == "iframe" || tag.name == "xmp" || tag.name == "noembed" || tag.name == "noframes" || tag.name == "noscript") {
			lower, marker := strings.ToLower(page[pos:]), "</"+tag.name
			search := 0
			for {
				end := strings.Index(lower[search:], marker)
				if end < 0 {
					return nil, errors.New("malformed GitHub settings raw text")
				}
				end += search
				after := end + len(marker)
				if after < len(lower) && strings.ContainsRune(" \t\r\n/>", rune(lower[after])) {
					pos += end
					break
				}
				search = after
			}
		}
	}
	return tags, nil
}

func claimPage(page string) (string, url.Values, error) {
	tags, err := claimTags(page)
	if err != nil {
		return "", nil, err
	}
	origin, _ := url.Parse("https://github.com/settings/admin")
	account := ""
	var form *claimTag
	var fields url.Values
	var found url.Values
	loginInputs := 0
	for _, tag := range tags {
		if tag.name == "meta" && !tag.closing && (tag.attrs["name"] == "user-login" || tag.attrs["name"] == "octolytics-actor-login") {
			value := tag.attrs["content"]
			if !claimAccountName(value) || account != "" && !strings.EqualFold(value, account) {
				return "", nil, errors.New("GitHub authenticated identity is missing or ambiguous")
			}
			account = value
		}
		if tag.name == "form" {
			if !tag.closing {
				if form != nil {
					return "", nil, errors.New("ambiguous nested GitHub forms")
				}
				copy := tag
				form, fields, loginInputs = &copy, make(url.Values), 0
			} else if form != nil {
				action, err := origin.Parse(form.attrs["action"])
				renamePath := err == nil && action.EscapedPath() == "/account/rename"
				if renamePath {
					if !claimAllowedURL(action) || action.RawQuery != "" || action.Fragment != "" || strings.ToLower(form.attrs["method"]) != "post" || found != nil || loginInputs != 1 || len(fields["authenticity_token"]) != 1 || fields.Get("authenticity_token") == "" {
						return "", nil, errors.New("GitHub rename form is missing required safeguards or is ambiguous")
					}
					if method := strings.ToLower(fields.Get("_method")); len(fields["_method"]) > 1 || method != "" && method != "post" && method != "put" && method != "patch" {
						return "", nil, errors.New("GitHub rename form uses an unexpected method")
					}
					found = fields
				}
				form, fields = nil, nil
			}
		}
		if tag.name == "input" && !tag.closing && form != nil {
			name := tag.attrs["name"]
			_, disabled := tag.attrs["disabled"]
			if name == "" || disabled {
				continue
			}
			if tag.attrs["form"] != "" {
				return "", nil, errors.New("GitHub form input has ambiguous ownership")
			}
			if name == "login" {
				loginInputs++
			} else if strings.ToLower(tag.attrs["type"]) == "hidden" {
				fields.Add(name, tag.attrs["value"])
			}
		}
	}
	if account == "" || form != nil {
		return "", nil, errors.New("GitHub session is not authenticated or settings markup is incomplete")
	}
	return account, found, nil
}
