// Package webhook delivers a notification to any HTTP endpoint.
//
// It is the channel-agnostic event layer: the integration point for anything
// that is not Feishu (custom dashboards, alert relays). It does not serialize
// domain.Message directly. Instead it projects the message onto a small
// whitelist DTO, because domain.Message carries internal structures
// (Credential, QuotaSnapshot, Snapshot.Err) whose fields are not part of any
// external contract and could drift into leaking an internal handle or a raw
// upstream string.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

// Notifier POSTs a whitelist projection of a domain.Message to a fixed URL.
type Notifier struct {
	url      string
	client   *http.Client
	renderer *render.Renderer
}

// New builds a webhook notifier. A nil client falls back to http.DefaultClient.
//
// An empty URL is not silently ignored here on purpose: deciding whether the
// webhook is wired at all belongs to the composition root. Callers that have no
// URL configured must simply not register this notifier.
func New(url string, client *http.Client) *Notifier {
	if client == nil {
		client = http.DefaultClient
	}
	return &Notifier{url: url, client: client, renderer: render.New("")}
}

// Name identifies the channel.
func (n *Notifier) Name() string { return "webhook" }

// Notify projects msg onto the whitelist payload and POSTs it. Any non-2xx
// response is an error; the body is truncated into the error to stay useful
// without echoing huge payloads.
func (n *Notifier) Notify(ctx context.Context, msg domain.Message) error {
	payload, err := json.Marshal(project(n.renderer, msg))
	if err != nil {
		return fmt.Errorf("webhook: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook: unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// payload is the entire external schema. Anything not listed here is not part
// of the contract and must not be added without a deliberate decision.
type payload struct {
	Title       string        `json:"title"`
	Kind        string        `json:"kind"`
	Body        string        `json:"body"`
	GeneratedAt string        `json:"generated_at,omitempty"`
	Degraded    bool          `json:"degraded,omitempty"`
	Notes       []string      `json:"notes,omitempty"`
	Providers   []providerDTO `json:"providers,omitempty"`
	Alerts      []alertDTO    `json:"alerts,omitempty"`
}

// providerDTO is a provider-level summary. It intentionally omits snapshots:
// their Credential and Err fields are internal.
type providerDTO struct {
	Provider   string      `json:"provider"`
	WorstState string      `json:"worst_state"`
	Healthy    int         `json:"healthy"`
	Total      int         `json:"total"`
	Error      string      `json:"error,omitempty"`
	Best       []windowDTO `json:"best_windows,omitempty"`
}

type windowDTO struct {
	Scope     domain.QuotaScope `json:"scope"`
	ScopeID   string            `json:"scope_id,omitempty"`
	Name      string            `json:"name"`
	UsedPct   *float64          `json:"used_percent,omitempty"`
	ResetAt   string            `json:"reset_at,omitempty"`
	ResetText string            `json:"reset_text,omitempty"`
}

// alertDTO carries only the agreed alert fields; the credential is reduced to
// its non-secret label.
type alertDTO struct {
	Scope      domain.QuotaScope `json:"scope,omitempty"`
	ScopeID    string            `json:"scope_id,omitempty"`
	Kind       string            `json:"kind"`
	Severity   string            `json:"severity"`
	Evidence   string            `json:"evidence"`
	Credential string            `json:"credential_label"`
	Title      string            `json:"title"`
	Detail     string            `json:"detail"`
	Facts      []string          `json:"facts,omitempty"`
	Advice     string            `json:"advice,omitempty"`
}

func project(r *render.Renderer, msg domain.Message) payload {
	p := payload{
		Title: r.Title(msg),
		Kind:  msg.Kind,
		Body:  r.Text(msg),
	}
	if msg.Report != nil {
		p.GeneratedAt = msg.Report.GeneratedAt.Format("2006-01-02T15:04:05Z07:00")
		p.Degraded = msg.Report.Degraded
		p.Notes = msg.Report.Notes
		for _, pr := range msg.Report.Providers {
			provider := providerDTO{
				Provider:   string(pr.Provider),
				WorstState: string(pr.WorstState),
				Healthy:    pr.Healthy,
				Total:      pr.Total,
				Error:      pr.Error,
			}
			for _, w := range pr.BestWindows {
				provider.Best = append(provider.Best, windowDTO{
					Scope: w.Scope.Normalized(), ScopeID: w.ScopeID,
					Name:      w.Name,
					UsedPct:   w.UsedPercent,
					ResetText: w.ResetText,
					ResetAt:   formatPtrTime(w.ResetAt),
				})
			}
			p.Providers = append(p.Providers, provider)
		}
	}
	for _, a := range msg.Alerts {
		p.Alerts = append(p.Alerts, alertDTO{
			Scope: a.Scope, ScopeID: a.ScopeID,
			Kind:       string(a.Kind),
			Severity:   string(a.Severity),
			Evidence:   string(a.Evidence),
			Credential: a.Credential.Label(),
			Title:      a.Title,
			Detail:     a.Detail,
			Facts:      a.Facts,
			Advice:     a.Advice,
		})
	}
	return p
}

func formatPtrTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z07:00")
}
