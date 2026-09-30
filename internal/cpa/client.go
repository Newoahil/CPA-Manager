// Package cpa collects quota through CPA management APIs. It does not change
// credential enablement or policy. Quota calls can trigger CPA's OAuth refresh.
package cpa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/quota"
)

const maxBodyBytes = 8 << 20

var (
	ErrUnauthorized = errors.New("cpa: 管理密钥无效")
	ErrUnsupported  = errors.New("cpa: 管理端点不可用，无法确认 API 版本或能力")
)

type Options struct {
	APIVersion         string
	QuotaStrategy      string
	AntigravityProfile string
	ContextOverrides   map[string]ContextOverride
}

type listCall struct {
	done  chan struct{}
	creds []domain.Credential
	err   error
}

const (
	gateWaitTimeout    = 30 * time.Second
	probeFlightTimeout = 5 * time.Minute
)

// Accessed only while holding requestGate. The two control-plane caches are
// separate from management authentication and from credential quota requests.
type retryState struct {
	at       time.Time
	status   int
	err      error
	failures int
}

func (r *retryState) record(status int, err error, minimum time.Duration) {
	r.failures = min(r.failures+1, 6)
	delay := min(30*time.Second*time.Duration(1<<(r.failures-1)), 15*time.Minute)
	r.at, r.status, r.err = time.Now().Add(max(delay, minimum)), status, err
}

// Client may be shared by the health watcher and concurrent collectors.
type Client struct {
	baseURL      string
	key          string
	http         *http.Client
	opts         Options
	optionErr    error
	mu           sync.Mutex
	version      string
	listing      *listCall
	contexts     map[string]credentialContext
	capabilities *capabilityCall
	requestGate  chan struct{}
	authRetry    retryState
	listRetry    retryState
	capRetry     retryState
	generation   uint64
}

func New(baseURL, managementKey string, timeout time.Duration) *Client {
	return NewWithOptions(baseURL, managementKey, timeout, Options{})
}

func NewWithOptions(baseURL, managementKey string, timeout time.Duration, opts Options) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if opts.APIVersion == "" {
		opts.APIVersion = "auto"
	}
	if opts.QuotaStrategy == "" {
		opts.QuotaStrategy = "auto"
	}
	if opts.AntigravityProfile == "" {
		opts.AntigravityProfile = "current"
	}
	c := &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		key:     strings.TrimSpace(managementKey), opts: opts,
		http:        &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		contexts:    make(map[string]credentialContext),
		requestGate: make(chan struct{}, 1),
	}
	c.opts.ContextOverrides = make(map[string]ContextOverride, len(opts.ContextOverrides))
	for key, value := range opts.ContextOverrides {
		if !validOverride(key, value) {
			c.optionErr = errors.New("cpa: invalid context overrides")
		}
		c.opts.ContextOverrides[key] = value
	}
	if !oneOf(opts.APIVersion, "auto", "v0", "v8") || !oneOf(opts.QuotaStrategy, "auto", "normalized", "proxy") || !oneOf(opts.AntigravityProfile, "current", "legacy") {
		c.optionErr = errors.New("cpa: invalid compatibility options")
	}
	return c
}

func oneOf(v string, values ...string) bool {
	for _, s := range values {
		if v == s {
			return true
		}
	}
	return false
}

// Ping validates the list schema, sharing in-flight listing/negotiation.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.ListCredentials(ctx)
	return err
}

// Timeout is the configured budget for one HTTP request, excluding gate wait.
func (c *Client) Timeout() time.Duration { return c.http.Timeout }

// FlightTimeout bounds a complete shared probe, including queueing/pagination.
func (c *Client) FlightTimeout() time.Duration { return probeFlightTimeout }

func (c *Client) APIVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *Client) Source() domain.Source {
	if c.APIVersion() == "v0" {
		return domain.SourceCPAV0
	}
	if c.APIVersion() == "v8" {
		return domain.SourceCPA
	}
	return ""
}

func (c *Client) ListCredentials(ctx context.Context) ([]domain.Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.optionErr != nil {
		return nil, c.optionErr
	}
	c.mu.Lock()
	if call := c.listing; call != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			return append([]domain.Credential{}, call.creds...), call.err
		}
	}
	call := &listCall{done: make(chan struct{})}
	c.listing = call
	version := c.version
	c.mu.Unlock()
	go c.runList(call, version)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return append([]domain.Credential{}, call.creds...), call.err
	}
}

func (c *Client) runList(call *listCall, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.FlightTimeout())
	defer cancel()
	creds, contexts, selected, err := c.list(ctx, version)
	c.mu.Lock()
	if err == nil {
		c.version, c.contexts = selected, contexts
		c.generation++
		if c.capabilities != nil {
			select {
			case <-c.capabilities.done:
				c.capabilities = nil
			default:
			}
		}
	}
	call.creds, call.err = creds, err
	c.listing = nil
	close(call.done)
	c.mu.Unlock()
}

func (c *Client) FetchQuota(ctx context.Context, cred domain.Credential) (domain.QuotaSnapshot, error) {
	snap := domain.QuotaSnapshot{Credential: cred, Source: c.Source(), Confidence: domain.ConfidenceUnknown, FetchedAt: time.Now()}
	if cred.Disabled {
		return fail(snap, domain.FailureUnsupported, "凭证已禁用，跳过额度请求"), nil
	}
	if strings.TrimSpace(cred.AuthIndex) == "" {
		return fail(snap, domain.FailureParse, "凭证缺少 auth_index"), nil
	}
	if c.APIVersion() == "" {
		if _, err := c.ListCredentials(ctx); err != nil {
			if ctx.Err() != nil {
				return snap, ctx.Err()
			}
			return fail(snap, domain.FailureControlPlane, "管理 API 协商失败"), nil
		}
	}
	snap.Source = c.Source()
	version := c.APIVersion()
	strategy := c.opts.QuotaStrategy
	if version == "v0" && strategy == "normalized" {
		return fail(snap, domain.FailureUnsupported, "v0 不支持统一额度接口，请使用 auto 或 proxy"), nil
	}
	c.mu.Lock()
	cc, found := c.contexts[cred.AuthIndex]
	c.mu.Unlock()
	if !found || cc.provider != cred.Provider {
		return fail(snap, domain.FailureParse, "凭证上下文不可用，请刷新列表"), nil
	}
	if cc.contextInvalid {
		return fail(snap, domain.FailureParse, "额度账户或项目上下文无效或冲突"), nil
	}
	if strategy == "auto" && version == "v8" {
		caps, err := c.getCapabilities(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return snap, ctx.Err()
			}
			kind := domain.FailureParse
			var statusErr managementStatusError
			var transportErr transportError
			if errors.As(err, &statusErr) {
				kind = classifyStatus(statusErr.status)
			} else if errors.As(err, &transportErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				kind = domain.FailureTransport
			}
			return fail(snap, kind, "额度能力查询失败；可显式配置 proxy 策略"), nil
		}
		if cc.capabilityInvalid {
			return fail(snap, domain.FailureParse, "凭证额度能力元数据无效"), nil
		}
		strategy = "proxy"
		if cc.normalized(caps) {
			strategy = "normalized"
		}
	}
	if strategy == "normalized" {
		raw, _ := json.Marshal(map[string]string{"auth_index": cred.AuthIndex})
		body, status, err := c.do(ctx, "v8", http.MethodPost, "/credentials/quota/fetch", raw)
		if err != nil {
			return snap, err
		}
		if status < 200 || status >= 300 {
			return c.failureFromStatus(snap, status, body), nil
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(body, &envelope) != nil {
			return fail(snap, domain.FailureParse, "统一额度响应结构无效"), nil
		}
		if e, ok := envelope["error"]; ok && string(e) != "null" {
			return fail(snap, domain.FailureParse, "统一额度接口返回错误"), nil
		}
		windows, plan, balance, _, _ := parseContract(body)
		if len(windows) == 0 {
			return fail(snap, domain.FailureParse, "统一额度响应没有有效测量"), nil
		}
		snap.Windows, snap.Plan, snap.Balance = windows, plan, balance
		snap.Confidence, snap.OK = domain.ConfidenceReported, true
		return snap, nil
	}
	if cc.contextInvalid {
		return fail(snap, domain.FailureParse, "额度账户或项目上下文缺失、无效或冲突"), nil
	}
	request, err := quota.Build(cc.quota)
	if err != nil {
		kind := domain.FailureParse
		if errors.Is(err, quota.ErrUnsupported) {
			kind = domain.FailureUnsupported
		}
		return fail(snap, kind, "额度 provider 不支持或必需上下文缺失"), nil
	}
	raw, _ := json.Marshal(struct {
		AuthIndex string            `json:"auth_index"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
		Data      string            `json:"data"`
	}{cred.AuthIndex, request.Method, request.URL, request.Header, request.Data})
	path := "/api-call"
	if version == "v8" {
		path = "/requests/api-call"
	}
	body, status, err := c.do(ctx, version, http.MethodPost, path, raw)
	if err != nil {
		return snap, err
	}
	if status < 200 || status >= 300 {
		return c.failureFromStatus(snap, status, body), nil
	}
	var envelope struct {
		Status *int                `json:"status_code"`
		Header map[string][]string `json:"header"`
		Body   *string             `json:"body"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Status == nil || *envelope.Status < 100 || *envelope.Status > 599 || envelope.Body == nil || envelope.Header == nil {
		return fail(snap, domain.FailureParse, "管理代理响应结构无效"), nil
	}
	r := quota.Parse(cc.quota, *envelope.Status, []byte(*envelope.Body), snap.FetchedAt)
	snap.Windows, snap.Plan, snap.Balance = r.Windows, r.Plan, r.Balance
	// Extra usage ships inside the same response; carrying it costs no request.
	snap.ExtraUsage = r.ExtraUsage
	snap.Confidence, snap.Failure, snap.Err = r.Confidence, r.Failure, r.Err
	if r.Failure != domain.FailureNone {
		snap.FailureScope = domain.ScopeUnknown
	}
	snap.OK = r.Failure == domain.FailureNone && len(r.Windows) > 0
	return snap, nil
}

func fail(s domain.QuotaSnapshot, kind domain.FailureKind, message string) domain.QuotaSnapshot {
	s.OK, s.Failure, s.Err, s.Confidence = false, kind, message, domain.ConfidenceUnknown
	s.Windows, s.Plan, s.Balance, s.ExtraUsage = nil, "", "", nil
	// A generic rejection supplies no verified applicability, even when its
	// kind is quota. Never reuse an earlier window or credential's scope.
	s.FailureScope, s.FailureScopeID = domain.ScopeUnknown, ""
	return s
}

func (c *Client) failureFromStatus(s domain.QuotaSnapshot, status int, _ []byte) domain.QuotaSnapshot {
	return fail(s, classifyStatus(status), fmt.Sprintf("额度请求失败（管理 HTTP %d）", status))
}

// transportError preserves errors.Is without exposing a URL or response text.
type transportError struct{ cause error }

func (e transportError) Error() string        { return "cpa: management transport failed" }
func (e transportError) Is(target error) bool { return errors.Is(e.cause, target) }

func (c *Client) do(ctx context.Context, version, method, path string, data []byte) ([]byte, int, error) {
	// Serialize management attempts so a failing request closes the gate before
	// any queued caller can hit CPA (including quota and capability requests).
	gateCtx, cancelGate := context.WithTimeout(ctx, gateWaitTimeout)
	defer cancelGate()
	select {
	case c.requestGate <- struct{}{}:
		defer func() { <-c.requestGate }()
	case <-gateCtx.Done():
		return nil, 0, transportError{gateCtx.Err()}
	}
	if err := gateCtx.Err(); err != nil {
		return nil, 0, transportError{err}
	}
	cancelGate() // Gate waiting does not consume the HTTP client's timeout.
	if err := ctx.Err(); err != nil {
		return nil, 0, transportError{err}
	}
	if time.Now().Before(c.authRetry.at) {
		return nil, c.authRetry.status, c.authRetry.err
	}
	var control *retryState
	if method == http.MethodGet {
		switch strings.SplitN(path, "?", 2)[0] {
		case "/credentials", "/auth-files":
			control = &c.listRetry
		case "/credentials/quota/providers":
			control = &c.capRetry
		}
	}
	if control != nil && time.Now().Before(control.at) {
		return nil, control.status, control.err
	}
	body, status, err := c.request(ctx, version, method, path, data)
	if ctx.Err() != nil {
		return body, status, err // Never cache caller/flight cancellation.
	}
	if status == 401 || status == 403 {
		c.authRetry.record(status, nil, 5*time.Minute)
	} else {
		c.authRetry = retryState{}
		if control != nil {
			var networkErr transportError
			if status == 429 || status >= 500 || errors.As(err, &networkErr) {
				control.record(status, err, 30*time.Second)
			} else {
				*control = retryState{}
			}
		}
	}
	return body, status, err
}

func (c *Client) request(ctx context.Context, version, method, path string, data []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+version+"/management"+path, bytes.NewReader(data))
	if err != nil {
		return nil, 0, transportError{err}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-Management-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, transportError{err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, transportError{err}
	}
	if len(body) > maxBodyBytes {
		return nil, resp.StatusCode, errors.New("cpa: management response too large")
	}
	return body, resp.StatusCode, nil
}

type managementStatusError struct{ status int }

func (e managementStatusError) Error() string {
	return fmt.Sprintf("cpa: management HTTP %d", e.status)
}

func (e managementStatusError) Is(target error) bool {
	return (target == ErrUnauthorized && (e.status == 401 || e.status == 403)) ||
		(target == ErrUnsupported && e.status == 404)
}

func statusError(status int) error {
	return managementStatusError{status: status}
}

func toCredential(f credentialFile) domain.Credential {
	index, provider := strings.TrimSpace(f.AuthIndex), string(mapProvider(f.Provider))
	return domain.Credential{Key: opaqueKey(provider, index), Provider: mapProvider(provider), Alias: safeAlias(f.Name), ShortID: shortID(index), AuthIndex: index, Status: strings.TrimSpace(f.Status), Disabled: f.Disabled, Unavailable: f.Unavailable}
}

func opaqueKey(provider, authIndex string) string {
	sum := sha256.Sum256([]byte(provider + ":" + authIndex))
	return provider + "/" + hex.EncodeToString(sum[:])[:16]
}

func mapProvider(raw string) domain.ProviderKind {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "anthropic" {
		p = "claude"
	}
	return domain.ProviderKind(p)
}

func safeAlias(name string) string {
	s := strings.TrimSpace(name)
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[:at]
	}
	r := []rune(s)
	if len(r) > 32 {
		r = r[:32]
	}
	return string(r)
}

func shortID(index string) string {
	if index == "" {
		return ""
	}
	r := []rune(index)
	if len(r) > 4 {
		r = r[len(r)-4:]
	}
	return "****" + string(r)
}
