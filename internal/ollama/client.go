// Package ollama scrapes Ollama Cloud quota from the account settings page.
//
// This is an UNOFFICIAL screen scrape. Ollama publishes no quota API, so the
// only known approach is to request https://ollama.com/settings with the
// account's `__Secure-session` cookie and parse the returned HTML. Any HTML
// redesign can silently break this parser; failures degrade to an unavailable
// snapshot rather than wrong numbers.
//
// SPDX-License-Identifier: LGPL-3.0-only
// Local modifications copyright (C) 2026 CPA Manager contributors.
//
// Parser approach adapted from jacklee-code/ollama-cloud-quota-monitor
// (https://github.com/jacklee-code/ollama-cloud-quota-monitor), whose parser in
// turn derives from Wei-Shaw/sub2api. This package is conservatively distributed
// as an adaptation under LGPL version 3, not claimed to be a clean-room rewrite.
// Local differences documented on 2026-09-29 include line-oriented DOM parsing,
// nearby-line window matching, relative resets and CPA Manager snapshots.
// THIRD_PARTY_NOTICES.md records upstream attribution and release-time comparison
// commits, which are not asserted to be the original development revisions.
// See LICENSE, COPYING.LESSER and COPYING for the license and warranty terms.
//
// SECURITY: the session cookie is a high-value secret. It must never appear in
// logs, error strings, snapshots (including Raw) or any returned value.
package ollama

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// settingsURL is the only page scraped. Kept as a field on Client so tests can
// point it at a local fixture server.
const settingsURL = "https://ollama.com/settings"

const maxBodyBytes = 4 << 20 // 4 MiB

// errSessionInvalid is the user-facing message for an expired/absent session.
const errSessionInvalid = "session cookie 已失效，需要重新获取"

// Client fetches and parses the Ollama settings page.
type Client struct {
	http *http.Client
	url  string
}

// New builds a Client. Redirects are deliberately NOT followed: a 3xx to the
// login page must be observed as an auth failure, not hidden by the transport.
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		url: settingsURL,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Fetch retrieves the quota view for one configured account.
//
// Transport errors return a non-nil error (wrapped with %w so callers can still
// detect context.DeadlineExceeded). Auth and parse failures return a snapshot
// with OK=false, a structured Failure and a scrubbed Err, never an error.
//
// Success requires at least one numeric quota window. plan/balance alone are
// not success: otherwise a redesigned settings page would look like a
// permanently healthy account and never age toward stale.
func (c *Client) Fetch(ctx context.Context, account config.OllamaAccount) (domain.QuotaSnapshot, error) {
	snap := domain.QuotaSnapshot{
		Credential: domain.Credential{
			Key:      "ollama/" + account.Name,
			Provider: domain.ProviderOllama,
			Alias:    account.Name,
			ShortID:  "****",
		},
		Source: domain.SourceOllamaWeb,
		// The settings page is not an official API, so even a successful parse
		// is a locally-derived number.
		Confidence: domain.ConfidenceEstimated,
		FetchedAt:  time.Now(),
	}
	if strings.TrimSpace(account.SecureSession) == "" {
		snap.Failure = domain.FailureAuth
		snap.Err = errSessionInvalid
		return snap, nil
	}
	body, status, err := c.get(ctx, account.SecureSession)
	if err != nil {
		return snap, err
	}
	switch {
	case status >= 300 && status < 400:
		snap.Failure = domain.FailureAuth
		snap.Err = errSessionInvalid
		return snap, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		snap.Failure = domain.FailureAuth
		snap.Err = errSessionInvalid
		return snap, nil
	case status != http.StatusOK:
		snap.Failure = domain.FailureTransport
		snap.Err = fmt.Sprintf("ollama settings 请求失败（HTTP %d）", status)
		return snap, nil
	}
	if looksLikeLogin(body) {
		snap.Failure = domain.FailureAuth
		snap.Err = errSessionInvalid
		return snap, nil
	}
	result := parse(string(body), snap.FetchedAt)
	snap.Plan = result.plan
	snap.Balance = result.balance
	if len(result.windows) == 0 {
		snap.Failure = domain.FailureParse
		snap.Err = "未能从 settings 页面解析出额度窗口（页面结构可能已变更）"
		return snap, nil
	}
	snap.Windows = result.windows
	snap.OK = true
	return snap, nil
}

// get performs the request with the cookie header and returns the body/status.
// The error never contains the cookie value.
func (c *Client) get(ctx context.Context, cookie string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("ollama: build settings request: %w", err)
	}
	// If the configured value already contains '=', it is a full Cookie header
	// (e.g. "a=1; b=2"); otherwise wrap it as the __Secure-session cookie.
	if strings.Contains(cookie, "=") {
		req.Header.Set("Cookie", cookie)
	} else {
		req.Header.Set("Cookie", "__Secure-session="+cookie)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ollama: GET settings: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("ollama: read settings: %w", err)
	}
	return body, resp.StatusCode, nil
}

// looksLikeLogin detects the login page without relying on a single marker.
func looksLikeLogin(body []byte) bool {
	s := strings.ToLower(string(body))
	markers := []string{
		`type="password"`,
		"sign in to ollama",
		"sign in with",
		"<title>sign in",
		"log in to ollama",
	}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
