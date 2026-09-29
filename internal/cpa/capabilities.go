package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type quotaCapability struct {
	PluginID           string   `json:"plugin_id"`
	Provider           string   `json:"provider"`
	DisplayName        string   `json:"display_name"`
	SupportedProviders []string `json:"supported_providers"`
	SupportsReset      *bool    `json:"supports_reset"`
}

type capabilityCall struct {
	generation uint64
	done       chan struct{}
	caps       []quotaCapability
	err        error
}

func (cc credentialContext) normalized(caps []quotaCapability) bool {
	if cc.supportsQuota || cc.probe || cc.quotaProvider != "" {
		return true
	}
	for _, cap := range caps {
		if mapProvider(cap.Provider) == cc.provider {
			return true
		}
		for _, p := range cap.SupportedProviders {
			if mapProvider(p) == cc.provider {
				return true
			}
		}
	}
	return false
}

func (c *Client) getCapabilities(ctx context.Context) ([]quotaCapability, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if call := c.capabilities; call != nil {
			if call.generation != c.generation {
				// A new list invalidates the result, not a still-running goroutine.
				// Wait for the old bounded flight before starting the new generation.
				select {
				case <-call.done:
					c.capabilities = nil
					c.mu.Unlock()
					continue
				default:
				}
				c.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-call.done:
					continue
				}
			}
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-call.done:
				return call.caps, call.err
			}
		}
		call := &capabilityCall{done: make(chan struct{}), generation: c.generation}
		c.capabilities = call
		c.mu.Unlock()
		go c.runCapabilities(call)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			return call.caps, call.err
		}
	}
}

func (c *Client) runCapabilities(call *capabilityCall) {
	ctx, cancel := context.WithTimeout(context.Background(), c.FlightTimeout())
	defer cancel()
	body, status, err := c.do(ctx, "v8", http.MethodGet, "/credentials/quota/providers", nil)
	var caps []quotaCapability
	if err == nil && status != http.StatusOK {
		err = statusError(status)
	}
	if err == nil {
		var payload struct {
			Providers []quotaCapability `json:"providers"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Providers == nil {
			err = errors.New("cpa: invalid quota capabilities schema")
		} else {
			caps = payload.Providers
			for _, cap := range caps {
				if cap.PluginID == "" || cap.Provider == "" || cap.DisplayName == "" || cap.SupportedProviders == nil || cap.SupportsReset == nil {
					err = errors.New("cpa: invalid quota capability object")
				}
				for _, p := range cap.SupportedProviders {
					if p == "" {
						err = errors.New("cpa: invalid supported provider")
					}
				}
			}
		}
	}
	c.mu.Lock()
	call.caps, call.err = caps, err
	close(call.done)
	c.mu.Unlock()
}

// QuotaProviders keeps the public signature while strictly validating the v8
// capability objects. It reports capability identifiers, not assumed adapters.
func (c *Client) QuotaProviders(ctx context.Context) ([]string, error) {
	if c.APIVersion() == "" {
		if _, err := c.ListCredentials(ctx); err != nil {
			return nil, err
		}
	}
	if c.APIVersion() != "v8" {
		return nil, ErrUnsupported
	}
	caps, err := c.getCapabilities(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(caps))
	for _, cap := range caps {
		out = append(out, cap.Provider)
	}
	return out, nil
}
