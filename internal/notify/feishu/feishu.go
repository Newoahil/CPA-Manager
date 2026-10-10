// Package feishu implements the Feishu (Lark) self-built-app bot: outbound
// notifications to one group plus a long-connection listener for @Bot queries
// and read-only card callbacks.
//
// Deployment constraint: run exactly ONE replica. Feishu allows at most 50
// concurrent long connections per app and delivers each event to only one of
// them (cluster delivery, not broadcast). A second replica would make replies
// and card interactions land on a process at random, so a message could be
// answered by an instance that does not hold the newest report.
//
// Authorization boundary: every group member may run read-only queries. There
// is no manual refresh: collection is entirely scheduler-driven, so a query only
// reads the last evaluated report. The card callback's Action.Value is input
// data, never a credential; it is parsed for its action name and nothing else.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

// defaultReplyBudget bounds a reply send so a slow Feishu API cannot block the
// message pipeline. It is deliberately below the 3-second callback budget so we
// fail soft rather than let Feishu time us out.
const defaultReplyBudget = 2500 * time.Millisecond

// Bot is the Feishu notifier and long-connection listener.
type Bot struct {
	cfg      config.Config
	source   domain.ReportSource
	renderer *render.Renderer
	sender   sender
	log      *slog.Logger

	// replyBudget bounds a reply send so a slow Feishu API cannot block the
	// message pipeline. It is deliberately below the 3-second callback budget so
	// we fail soft instead of letting Feishu time us out. Overridable in tests.
	replyBudget time.Duration

	// inbound is the in-memory set of recently handled event keys. Feishu
	// delivers an event at least once, and a long-connection reconnect can
	// redeliver one already handled, so without this a single "@Bot 额度" can
	// draw several identical replies. It is deliberately not persisted: a
	// redelivery only matters within the short window the set covers.
	inbound *dedup

	// tasks owns the goroutines that run a query's slow half after the event
	// handler has returned. Feishu re-delivers an event whose handler is slow,
	// so the reply cannot run on the handler's own stack.
	tasks *taskGroup

	// querySlots bounds how many queries may render at once. A full channel is
	// the concurrency cap; the query is refused rather than queued.
	querySlots chan struct{}
}

var _ domain.Notifier = (*Bot)(nil)

// New builds the bot. It fails fast when the app credentials or target group
// are missing: a bot with no target group can never deliver anything, and
// silently accepting it would hide a configuration mistake.
func New(cfg config.Config, source domain.ReportSource) (*Bot, error) {
	if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" {
		return nil, errors.New("feishu: FEISHU_APP_ID and FEISHU_APP_SECRET are required")
	}
	if cfg.FeishuChatID == "" {
		return nil, errors.New("feishu: FEISHU_CHAT_ID is required")
	}
	return &Bot{
		cfg:         cfg,
		source:      source,
		renderer:    render.New(cfg.Tone).WithLocation(cfg.Location).WithCharts(cfg.CardChartsEnabled).WithIgnoredGroups(cfg.QuotaIgnoredGroups),
		sender:      newLarkSender(cfg.FeishuAppID, cfg.FeishuAppSecret),
		log:         slog.Default().With("component", "feishu"),
		replyBudget: defaultReplyBudget,
		inbound:     newDedup(dedupCapacity, dedupTTL),
		tasks:       newTaskGroup(),
		querySlots:  make(chan struct{}, maxInflightQueries),
	}, nil
}

// Close cancels any in-flight query tasks and waits for them to unwind. It is
// called by Run when the connection stops; tests call it directly. It is safe to
// call more than once.
func (b *Bot) Close() {
	if b.tasks != nil {
		b.tasks.Close()
	}
}

// Name identifies the channel.
func (b *Bot) Name() string { return "feishu" }

// Notify renders msg as an interactive card and posts it to the configured
// group.
//
// The full card (chart + collapsible_panel) is tried first, then the simplified
// card. Feishu's JSON 2.0 is strict: a single unsupported component or property
// fails the whole send, and the chart's client-side rendering is the least
// certain element, so one bounded retry is worth more than a lost notification.
func (b *Bot) Notify(ctx context.Context, msg domain.Message) error {
	if _, err := b.sendCardWithFallback(ctx, b.cfg.FeishuChatID, msg); err != nil {
		return fmt.Errorf("feishu: send notification: %w", err)
	}
	return nil
}

// sendCardWithFallback posts the full card to chatID, and on failure logs the
// reason and posts the simplified card. It returns the variant that succeeded
// ("full" or "simple") so callers can record which path was taken.
func (b *Bot) sendCardWithFallback(ctx context.Context, chatID string, msg domain.Message) (string, error) {
	full := b.renderer.Card(msg)
	if err := b.sender.SendCard(ctx, chatID, full); err == nil {
		return "full", nil
	} else {
		b.log.WarnContext(ctx, "feishu full card rejected, retrying simplified",
			"action", "notify", "result", "fallback", "error", err)
	}
	simple := b.renderer.CardSimple(msg)
	if err := b.sender.SendCard(ctx, chatID, simple); err != nil {
		return "", fmt.Errorf("full and simplified card both failed: %w", err)
	}
	b.log.InfoContext(ctx, "feishu simplified card sent",
		"action", "notify", "result", "degraded", "charts", false)
	return "simple", nil
}

// replyCard posts a card as a reply, with the same full-then-simplified retry
// and the same secret-free logging as an outbound notification.
func (b *Bot) replyCard(ctx context.Context, messageID string, msg domain.Message) error {
	rctx, cancel := context.WithTimeout(ctx, b.replyBudget)
	defer cancel()
	full := b.renderer.Card(msg)
	if err := b.sender.ReplyCard(rctx, messageID, full); err == nil {
		return nil
	} else {
		b.log.WarnContext(ctx, "feishu full card reply rejected, retrying simplified",
			"action", "query", "result", "fallback", "error", err)
	}
	simple := b.renderer.CardSimple(msg)
	if err := b.sender.ReplyCard(rctx, messageID, simple); err != nil {
		return fmt.Errorf("full and simplified card reply both failed: %w", err)
	}
	b.log.InfoContext(ctx, "feishu simplified card reply sent",
		"action", "query", "result", "degraded", "charts", false)
	return nil
}

// patchCard updates an existing card message, with the same full-then-simplified retry
// and secret-free logging as sendCardWithFallback.
func (b *Bot) patchCard(ctx context.Context, messageID string, msg domain.Message) error {
	rctx, cancel := context.WithTimeout(ctx, b.replyBudget)
	defer cancel()
	full := b.renderer.Card(msg)
	if err := b.sender.PatchCard(rctx, messageID, full); err == nil {
		return nil
	} else {
		b.log.WarnContext(ctx, "feishu full card patch rejected, retrying simplified",
			"action", "card_action", "result", "fallback", "error", err)
	}
	simple := b.renderer.CardSimple(msg)
	if err := b.sender.PatchCard(rctx, messageID, simple); err != nil {
		return fmt.Errorf("full and simplified card patch both failed: %w", err)
	}
	b.log.InfoContext(ctx, "feishu simplified card patched",
		"action", "card_action", "result", "degraded", "charts", false)
	return nil
}
