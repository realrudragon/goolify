package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func startLocalSMTPServer(t *testing.T) (host string, port int, getReceived func() string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var captured strings.Builder
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				w := bufio.NewWriter(c)
				writeLine := func(s string) {
					_, _ = w.WriteString(s + "\r\n")
					_ = w.Flush()
				}
				writeLine("220 localhost ESMTP ready")
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					trimmed := strings.TrimRight(line, "\r\n")
					if inData {
						if trimmed == "." {
							inData = false
							writeLine("250 OK queued")
							continue
						}
						mu.Lock()
						captured.WriteString(trimmed + "\n")
						mu.Unlock()
						continue
					}
					upper := strings.ToUpper(trimmed)
					switch {
					case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
						writeLine("250-localhost")
						writeLine("250 OK")
					case strings.HasPrefix(upper, "MAIL FROM:"):
						writeLine("250 OK")
					case strings.HasPrefix(upper, "RCPT TO:"):
						writeLine("250 OK")
					case upper == "DATA":
						inData = true
						writeLine("354 End data with <CR><LF>.<CR><LF>")
					case upper == "QUIT":
						writeLine("221 Bye")
						return
					default:
						writeLine("250 OK")
					}
				}
			}(conn)
		}
	}()
	tcpAddr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", tcpAddr.Port, func() string {
		mu.Lock()
		defer mu.Unlock()
		return captured.String()
	}, func() { _ = ln.Close() }
}

func TestPhase3_AuthOptionsPasswordResetEmailOAuthCodeExchangeAndBoarding(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()

	// 1. Real SMTP password reset email delivery + token consumption + login verification
	smtpHost, smtpPort, getMail, stopSMTP := startLocalSMTPServer(t)
	defer stopSMTP()

	_, err := pool.Exec(ctx, `
		UPDATE instance_settings
		SET smtp_enabled = TRUE, smtp_host = $1, smtp_port = $2, smtp_from_address = 'noreply@coolify.test', fqdn = $3
		WHERE id = 0
	`, smtpHost, smtpPort, ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Verify /app/auth/options reports email_reset_enabled=true
	optResp, err := http.Get(ts.URL + "/app/auth/options")
	if err != nil {
		t.Fatal(err)
	}
	var opts struct {
		IsRegEnabled      bool     `json:"is_registration_enabled"`
		EmailResetEnabled bool     `json:"email_reset_enabled"`
		OAuthProviders    []string `json:"oauth_providers"`
	}
	_ = json.NewDecoder(optResp.Body).Decode(&opts)
	optResp.Body.Close()
	if optResp.StatusCode != http.StatusOK || !opts.EmailResetEnabled {
		t.Fatalf("expected /app/auth/options email_reset_enabled=true, got %d %+v", optResp.StatusCode, opts)
	}

	fpResp := doJSON(t, http.MethodPost, ts.URL+"/forgot-password", nil, "", "", map[string]string{
		"email": "root@coolify.test",
	})
	fpResp.Body.Close()
	if fpResp.StatusCode != http.StatusOK {
		t.Fatalf("forgot-password failed: %d", fpResp.StatusCode)
	}

	mailBody := getMail()
	re := regexp.MustCompile(`reset_token=([a-f0-9]+)`)
	m := re.FindStringSubmatch(mailBody)
	if len(m) < 2 {
		t.Fatalf("expected SMTP email to contain reset_token link, got:\n%s", mailBody)
	}
	resetTok := m[1]

	// Complete password reset
	rpResp := doJSON(t, http.MethodPost, ts.URL+"/reset-password", nil, "", "", map[string]string{
		"token":                 resetTok,
		"password":              "NewStrongPass456!",
		"password_confirmation": "NewStrongPass456!",
	})
	rpResp.Body.Close()
	if rpResp.StatusCode != http.StatusOK {
		t.Fatalf("reset-password failed: %d", rpResp.StatusCode)
	}

	// Replay of consumed reset token must fail with 422
	rpReplay := doJSON(t, http.MethodPost, ts.URL+"/reset-password", nil, "", "", map[string]string{
		"token":                 resetTok,
		"password":              "AnotherPass789!",
		"password_confirmation": "AnotherPass789!",
	})
	rpReplay.Body.Close()
	if rpReplay.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 replaying consumed token, got %d", rpReplay.StatusCode)
	}

	// Verify login works with the NEW password
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "NewStrongPass456!")

	// 2. Real OAuth2 authorization-code exchange (POST /token -> Bearer GET /userinfo)
	var tokenExchanged bool
	oauthMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			_ = r.ParseForm()
			if r.FormValue("grant_type") != "authorization_code" || r.FormValue("code") != "real-code-99" || r.FormValue("client_secret") != "my-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			tokenExchanged = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"oauth-access-token-777","token_type":"Bearer"}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer oauth-access-token-777" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sub":"gh-user-42","email":"oauthuser@coolify.test","name":"OAuth User"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer oauthMock.Close()

	_, err = pool.Exec(ctx, `
		INSERT INTO oauth_settings (provider, enabled, client_id, client_secret, redirect_uri, tenant, allow_registration)
		VALUES ('github', TRUE, 'cid-1', 'my-secret', $1, $2, TRUE)
		ON CONFLICT (provider) DO UPDATE SET enabled = TRUE, client_id = 'cid-1', client_secret = 'my-secret', redirect_uri = EXCLUDED.redirect_uri, tenant = EXCLUDED.tenant, allow_registration = TRUE
	`, ts.URL+"/auth/github/callback", oauthMock.URL)
	if err != nil {
		t.Fatal(err)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirResp, err := noRedirect.Get(ts.URL + "/auth/github/redirect")
	if err != nil {
		t.Fatal(err)
	}
	loc := redirResp.Header.Get("Location")
	redirResp.Body.Close()
	if redirResp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, oauthMock.URL+"/authorize?") {
		t.Fatalf("expected 302 redirect to OAuth authorize URL, got %d %q", redirResp.StatusCode, loc)
	}
	parsedLoc, _ := url.Parse(loc)
	state := parsedLoc.Query().Get("state")

	cbResp, err := noRedirect.Get(ts.URL + "/auth/github/callback?state=" + url.QueryEscape(state) + "&code=real-code-99")
	if err != nil {
		t.Fatal(err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK && cbResp.StatusCode != http.StatusFound {
		t.Fatalf("OAuth callback failed: %d", cbResp.StatusCode)
	}
	if !tokenExchanged {
		t.Fatal("expected real RFC 6749 POST /token authorization_code exchange to be executed")
	}

	// 3. Onboarding completion
	_, _ = pool.Exec(ctx, `UPDATE teams SET show_boarding = TRUE WHERE id = 0`)
	bResp := doJSON(t, http.MethodPost, ts.URL+"/app/boarding/complete", cookies, csrf, "", nil)
	bResp.Body.Close()
	if bResp.StatusCode != http.StatusOK {
		t.Fatalf("boarding/complete failed: %d", bResp.StatusCode)
	}
	var showBoarding bool
	if err := pool.QueryRow(ctx, `SELECT show_boarding FROM teams WHERE id = 0`).Scan(&showBoarding); err != nil || showBoarding {
		t.Fatalf("expected show_boarding=false, got %v err=%v", showBoarding, err)
	}
}
