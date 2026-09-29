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
// Authorization boundary: every group member may run read-only queries, and
// the refresh button only re-collects. The card callback's Action.Value is
// input data, never a credential; it is parsed for its action name and nothing
// else.
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
	cfg       config.Config
	refresher domain.QuotaRefresher
	renderer  *render.Renderer
	sender    sender
	log       *slog.Logger

	// replyBudget bounds a reply send so a slow Feishu API cannot block the
	// message pipeline. It is deliberately below the 3-second callback budget so
	// we fail soft instead of letting Feishu time us out. Overridable in tests.
	replyBudget time.Duration
	// refreshBudget bounds a read-only re-collection inside a callback so the
	// 3-second callback budget is never exceeded. Overridable in tests.
	refreshBudget time.Duration
}

var _ domain.Notifier = (*Bot)(nil)

// New builds the bot. It fails fast when the app credentials or target group
// are missing: a bot with no target group can never deliver anything, and
// silently accepting it would hide a configuration mistake.
func New(cfg config.Config, refresher domain.QuotaRefresher) (*Bot, error) {
	if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" {
		return nil, errors.New("feishu: FEISHU_APP_ID and FEISHU_APP_SECRET are required")
	}
	if cfg.FeishuChatID == "" {
		return nil, errors.New("feishu: FEISHU_CHAT_ID is required")
	}
	return &Bot{
		cfg:           cfg,
		refresher:     refresher,
		renderer:      render.New(cfg.Tone).WithLocation(cfg.Location),
		sender:        newLarkSender(cfg.FeishuAppID, cfg.FeishuAppSecret),
		log:           slog.Default().With("component", "feishu"),
		replyBudget:   defaultReplyBudget,
		refreshBudget: refreshBudget,
	}, nil
}

// Name identifies the channel.
func (b *Bot) Name() string { return "feishu" }

// Notify renders msg as an interactive card and posts it to the configured
// group.
func (b *Bot) Notify(ctx context.Context, msg domain.Message) error {
	card := b.renderer.Card(msg)
	if err := b.sender.SendCard(ctx, b.cfg.FeishuChatID, card); err != nil {
		return fmt.Errorf("feishu: send notification: %w", err)
	}
	return nil
}
