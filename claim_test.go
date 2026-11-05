package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type claimTestTransport func(*http.Request) (*http.Response, error)

func (f claimTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func claimTestCookieFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func claimTestClaimer(t *testing.T) *githubClaimer {
	t.Helper()
	path := claimTestCookieFile(t, `[{"domain":".github.com","name":"user_session","value":"private-session-secret","path":"/","secure":true,"httpOnly":true}]`)
	g, err := newGitHubClaimer(path, "existing-account")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func claimTestHTML(account string) string {
	return `<html><head><meta name="user-login" content="` + account + `"></head><body><form action="/account/rename" method="post"><input type="hidden" name="authenticity_token" value="csrf-secret&amp;value"><input type="hidden" name="_method" value="patch"><input type="hidden" name="preserve_me" value="yes"><input name="login" type="text" value="existing-account"></form></body></html>`
}

func claimTestResponse(code int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/html; charset=utf-8")
	return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func TestGitHubClaimSyntheticFlowSubmitsExactlyOnePost(t *testing.T) {
	g := claimTestClaimer(t)
	gets, posts := 0, 0
	g.http.Transport = claimTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "github.com" || req.Header.Get("Authorization") != "" {
			t.Fatal("request escaped fixed GitHub origin or used API authentication")
		}
		if cookie, err := req.Cookie("user_session"); err != nil || cookie.Value != "private-session-secret" {
			t.Fatal("explicit session cookie missing")
		}
		switch req.Method {
		case http.MethodGet:
			gets++
			if req.URL.Path != "/settings/admin" {
				t.Fatal("unexpected GET destination")
			}
			account := "existing-account"
			if posts == 1 {
				account = "abc"
			}
			return claimTestResponse(200, claimTestHTML(account), nil), nil
		case http.MethodPost:
			posts++
			if gets != 1 || req.URL.Path != "/account/rename" || req.Header.Get("Origin") != "https://github.com" || req.Header.Get("Referer") != "https://github.com/settings/admin" {
				t.Fatal("POST occurred before account validation or used incorrect destination/context")
			}
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.PostForm.Get("login") != "abc" || req.PostForm.Get("authenticity_token") != "csrf-secret&value" || req.PostForm.Get("_method") != "patch" || req.PostForm.Get("preserve_me") != "yes" {
				t.Fatalf("form fields were not preserved safely: keys=%d", len(req.PostForm))
			}
			return claimTestResponse(303, "", http.Header{"Location": []string{"/settings/admin"}}), nil
		}
		t.Fatal("unexpected request method")
		return nil, errors.New("unexpected request")
	})
	if err := g.Claim(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if gets != 2 || posts != 1 {
		t.Fatalf("got %d GETs and %d POSTs", gets, posts)
	}
}

func TestGitHubClaimUnsafePreconditionsNeverPost(t *testing.T) {
	good := claimTestHTML("existing-account")
	for _, tc := range []struct{ name, body string }{
		{"wrongAccount", claimTestHTML("different-account")},
		{"noIdentity", strings.ReplaceAll(good, `<meta name="user-login" content="existing-account">`, "")},
		{"ambiguousIdentity", strings.ReplaceAll(good, "</head>", `<meta name="octolytics-actor-login" content="different-account"></head>`)},
		{"missingCSRF", strings.ReplaceAll(good, `<input type="hidden" name="authenticity_token" value="csrf-secret&amp;value">`, "")},
		{"crossOriginAction", strings.ReplaceAll(good, `action="/account/rename"`, `action="https://example.com/account/rename"`)},
		{"wrongAction", strings.ReplaceAll(good, `action="/account/rename"`, `action="/account/delete"`)},
		{"wrongMethod", strings.ReplaceAll(good, `method="post"`, `method="get"`)},
		{"dangerousOverride", strings.ReplaceAll(good, `value="patch"`, `value="delete"`)},
		{"duplicateOverride", strings.ReplaceAll(good, `value="patch">`, `value="patch"><input type="hidden" name="_method" value="delete">`)},
		{"duplicateCSRF", strings.ReplaceAll(good, `<input name="login"`, `<input type="hidden" name="authenticity_token" value="other"><input name="login"`)},
		{"duplicateForms", good + good},
		{"formInScript", `<meta name="user-login" content="existing-account"><script>` + good + `</script>`},
		{"scriptFalseClosingPrefix", `<script></scriptx>` + good + `</script>`},
		{"commentOnlyForm", `<meta name="user-login" content="existing-account"><!--` + good + `-->`},
		{"noForm", `<meta name="user-login" content="existing-account">`},
		{"duplicateAttributes", strings.ReplaceAll(good, `method="post"`, `method="post" method="get"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := claimTestClaimer(t)
			posts := 0
			g.http.Transport = claimTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					posts++
				}
				return claimTestResponse(200, tc.body, nil), nil
			})
			err := g.Claim(context.Background(), "abc")
			if err == nil || posts != 0 {
				t.Fatalf("unsafe precondition allowed mutation: err=%v posts=%d", err, posts)
			}
			if strings.Contains(err.Error(), "private-session-secret") || strings.Contains(err.Error(), "csrf-secret") {
				t.Fatal("error exposed a secret")
			}
		})
	}
}

func TestGitHubClaimInvalidTargetsNeverRequest(t *testing.T) {
	for _, name := range []string{"", "ab", "abcde", "Abc", "ab&", "../", "existing-account"} {
		g := claimTestClaimer(t)
		calls := 0
		g.http.Transport = claimTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("unexpected request")
		})
		if err := g.Claim(context.Background(), name); err == nil || calls != 0 {
			t.Fatalf("invalid target sent request: calls=%d", calls)
		}
	}
}

func TestGitHubClaimPostFailureIsNeverRetried(t *testing.T) {
	for _, mode := range []string{"network", "http500", "unconfirmed", "redirect307", "redirectLogin", "redirectExternal"} {
		t.Run(mode, func(t *testing.T) {
			g := claimTestClaimer(t)
			posts := 0
			g.http.Transport = claimTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return claimTestResponse(200, claimTestHTML("existing-account"), nil), nil
				}
				posts++
				switch mode {
				case "network":
					return nil, errors.New("transport error private-session-secret")
				case "http500":
					return claimTestResponse(500, "secret response body", nil), nil
				case "redirect307":
					return claimTestResponse(307, "", http.Header{"Location": []string{"/account/rename"}}), nil
				case "redirectLogin":
					return claimTestResponse(303, "", http.Header{"Location": []string{"/login"}}), nil
				case "redirectExternal":
					return claimTestResponse(303, "", http.Header{"Location": []string{"https://example.com/"}}), nil
				}
				return claimTestResponse(200, "", nil), nil
			})
			err := g.Claim(context.Background(), "abc")
			if err == nil || posts != 1 || !strings.Contains(err.Error(), "check") {
				t.Fatalf("ambiguous mutation did not stop: err=%v posts=%d", err, posts)
			}
			if strings.Contains(err.Error(), "private-session-secret") || strings.Contains(err.Error(), "secret response body") {
				t.Fatal("mutation error exposed sensitive response details")
			}
		})
	}
}

func TestGitHubClaimRejectsCredentialRedirectsBeforePost(t *testing.T) {
	for _, destination := range []string{"https://example.com/settings/admin", "http://github.com/settings/admin", "https://api.github.com/settings/admin", "https://github.com/login", "https://github.com/sudo", "https://github.com/two-factor/app"} {
		g := claimTestClaimer(t)
		calls := 0
		g.http.Transport = claimTestTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			return claimTestResponse(302, "", http.Header{"Location": []string{destination}}), nil
		})
		if err := g.Claim(context.Background(), "abc"); err == nil || calls != 1 {
			t.Fatalf("credential redirect followed: calls=%d err=%v", calls, err)
		}
	}
}

func TestClaimCookieExportsFilterDomainsSecurityAndExpiry(t *testing.T) {
	now := time.Now()
	for _, data := range []string{
		`[{"domain":"github.com","name":"yes","value":"private","secure":true},{"domain":"evilgithub.com","name":"no1","value":"private","secure":true},{"domain":".example.com","name":"no2","value":"private","secure":true},{"domain":"github.com","name":"no3","value":"private","secure":false},{"domain":"github.com","name":"expired","value":"private","secure":true,"expires":1}]`,
		"# Netscape HTTP Cookie File\n#HttpOnly_.github.com\tTRUE\t/\tTRUE\t0\tyes\tprivate\n.example.com\tTRUE\t/\tTRUE\t0\tno\tprivate\n",
		`{"cookies":[{"domain":".github.com","name":"yes","value":"private","secure":true}]}`,
	} {
		cookies, err := claimCookies([]byte(data), now)
		if err != nil || len(cookies) != 1 || cookies[0].Name != "yes" || !cookies[0].Secure {
			t.Fatalf("cookie export was not safely filtered: count=%d err=%v", len(cookies), err)
		}
	}
	for _, data := range []string{"", `[]`, `{"value":"private"}`, `[{"domain":"github.com","name":"x","value":"private","secure":false}]`, "bad\tfile"} {
		if _, err := claimCookies([]byte(data), now); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid export accepted or secret leaked")
		}
	}
}

func TestClaimConstructorPermissionsAndErrorsArePrivate(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "sensitive-cookie-filename")
	if _, err := newGitHubClaimer(privatePath, "existing-account"); err == nil || strings.Contains(err.Error(), privatePath) {
		t.Fatal("missing-file error leaked cookie path")
	}
	if runtime.GOOS != "windows" {
		path := claimTestCookieFile(t, `[]`)
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := newGitHubClaimer(path, "existing-account"); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatal("publicly readable cookie file accepted")
		}
	}
	if _, err := newGitHubClaimer("unused", "bad--account"); err == nil {
		t.Fatal("invalid expected account accepted")
	}
	if u, _ := url.Parse("https://github.com:443/account/rename"); claimAllowedURL(u) {
		t.Fatal("noncanonical origin accepted")
	}
}
