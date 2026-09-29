package cpa

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// This is the normalized v8 plugin/probe contract, not a built-in provider
// adapter. Unknown shapes and invalid measurements are never guessed/clamped.
type quotaFetchResponse struct {
	Subscription *struct {
		Plan     string `json:"plan"`
		TierName string `json:"tierName"`
	} `json:"subscription"`
	Summary []quotaMetric `json:"summary"`
	Groups  []struct {
		DisplayName string `json:"displayName"`
		Buckets     []struct {
			Window            string   `json:"window"`
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         *string  `json:"resetTime"`
		} `json:"buckets"`
	} `json:"groups"`
}

type quotaMetric struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Value    *float64 `json:"value"`
	Unit     string   `json:"unit"`
	Format   string   `json:"format"`
	Currency string   `json:"currency"`
}

// clamped is retained in the internal signature for clarity during migration;
// it is always false. An invalid declared number rejects the entire response.
func parseContract(body []byte) (windows []domain.QuotaWindow, plan, balance string, clamped, shaped bool) {
	var resp quotaFetchResponse
	if json.Unmarshal(body, &resp) != nil {
		return nil, "", "", false, false
	}
	shaped = resp.Subscription != nil || len(resp.Summary) > 0 || resp.Groups != nil
	if !shaped {
		return nil, "", "", false, false
	}
	if resp.Subscription != nil {
		plan = strings.TrimSpace(resp.Subscription.Plan)
		if plan == "" {
			plan = strings.TrimSpace(resp.Subscription.TierName)
		}
	}
	for _, m := range resp.Summary {
		hay := strings.ToLower(m.Key + " " + m.Label + " " + m.Unit + " " + m.Format)
		if m.Value != nil && balance == "" && (strings.Contains(hay, "balance") || strings.Contains(hay, "credit")) {
			balance = strconv.FormatFloat(*m.Value, 'f', -1, 64)
			if m.Currency != "" {
				balance += " " + m.Currency
			}
		}
	}
	seen := make(map[string]int)
	for _, g := range resp.Groups {
		for _, b := range g.Buckets {
			if b.RemainingFraction == nil {
				return nil, "", "", false, true
			}
			f := *b.RemainingFraction
			if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
				return nil, "", "", false, true
			}
			// Keep group/window identities distinct even when window names repeat.
			name := "groups/" + url.PathEscape(g.DisplayName) + "/windows/" + url.PathEscape(b.Window)
			seen[name]++
			if seen[name] > 1 {
				name += fmt.Sprintf("#%d", seen[name])
			}
			pct := math.Round((1-f)*100*1e6) / 1e6
			// v8.0.3 names identify windows, not their applicability. Keep the
			// measurement without inventing an account/model/group scope.
			w := domain.QuotaWindow{Scope: domain.ScopeUnknown, Name: name, UsedPercent: &pct}
			if b.ResetTime != nil && *b.ResetTime != "" {
				at, err := time.Parse(time.RFC3339Nano, *b.ResetTime)
				if err != nil {
					return nil, "", "", false, true
				}
				w.ResetAt = &at
			}
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, "", "", false, shaped
	}
	return windows, plan, balance, false, shaped
}
