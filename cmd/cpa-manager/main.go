// Command cpa-manager is a quota watcher for a running CLI Proxy API
// deployment.
//
// It observes credential quota across CPA-managed providers plus Ollama Cloud,
// raises Feishu alerts when a credential is exhausted, invalid or suspect, and
// offers provider-level capacity advice. It never changes credential enablement
// or policy; quota requests may trigger CPA's internal OAuth refresh.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	// Embed the timezone database so Asia/Shanghai resolves in a scratch
	// container without tzdata installed.
	_ "time/tzdata"

	"github.com/Newoahil/CPA-Manager/internal/app"
	"github.com/Newoahil/CPA-Manager/internal/calendar"
	"github.com/Newoahil/CPA-Manager/internal/collect"
	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/notify/feishu"
	"github.com/Newoahil/CPA-Manager/internal/notify/webhook"
	"github.com/Newoahil/CPA-Manager/internal/schedule"
	"github.com/Newoahil/CPA-Manager/internal/state"
	"github.com/Newoahil/CPA-Manager/internal/web"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// CPA reachability backoff bounds. The first probe is deliberately close to
// startup so an operator sees the outcome quickly; the ceiling keeps a
// long outage from hammering CPA.
const (
	pingBaseInterval = 30 * time.Second
	pingMaxInterval  = 15 * time.Minute
	pingAuthInterval = 5 * time.Minute
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The state volume must be writable before anything depends on durability.
	// A read-only /data makes every Save fail silently in the sense that the
	// process keeps running while re-alerting the same startup summary forever,
	// so this is a fatal, explicit check rather than a warning.
	if err := ensureStateWritable(cfg.StatePath); err != nil {
		return fmt.Errorf("state path %q is not writable: %w", cfg.StatePath, err)
	}
	log.Info("state path is writable", "path", cfg.StatePath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cpaClient := collect.NewCPAClient(cfg)

	store := state.NewFileStore(cfg.StatePath)
	engine := evaluate.New(cfg, calendar.New(cfg.Location))
	application := app.New(cfg, collect.AllWithCPA(cfg, cpaClient), engine, store, log)
	// Collector failures carry this sentinel when CPA itself is the problem, so
	// app can report a control-plane outage instead of blaming credentials.
	application.SetControlPlaneError(collect.ErrControlPlane)

	notifiers, bot := buildNotifiers(cfg, application, log)
	application.SetNotifiers(notifiers)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           web.NewHandler(application, version),
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// CPA reachability is watched, not required at boot. Exiting here would let
	// restart:unless-stopped loop on a bad management key and trip CPA's
	// five-strikes IP ban; instead the watcher runs degraded and backs off.
	spawn(&wg, func() { watchCPA(ctx, cpaClient, cfg.CPABaseURL, log) })

	spawn(&wg, func() {
		if err := application.RunPoll(ctx); err != nil {
			errCh <- err
		}
	})

	spawn(&wg, func() {
		scheduler := schedule.New(cfg.Location, cfg.SummaryTimes, application.SendSummary)
		if err := scheduler.Run(ctx); err != nil {
			errCh <- err
		}
	})

	spawn(&wg, func() {
		log.Info("status page listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	})

	if bot != nil {
		// The Feishu long connection is cluster-delivered, not broadcast: only
		// one replica may run it or button clicks land on an instance without
		// the latest report.
		spawn(&wg, func() {
			if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- err
			}
		})
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		log.Error("component failed, shutting down", "err", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	wg.Wait()
	return nil
}

// ensureStateWritable is the startup write probe: it creates and removes a
// temporary file next to the state file. A brand-new named volume is owned by
// root, and a distroless nonroot process would otherwise start successfully and
// fail every Save.
func ensureStateWritable(statePath string) error {
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".cpa-manager-write-probe-*")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	name := f.Name()
	_, writeErr := f.WriteString("ok")
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if writeErr != nil {
		return fmt.Errorf("write temp file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close temp file: %w", closeErr)
	}
	if removeErr != nil {
		return fmt.Errorf("remove temp file: %w", removeErr)
	}
	return nil
}

// watchCPA probes the CPA management API with exponential backoff and never
// aborts the process. Repeated management-auth failures trip a temporary
// source-IP ban on the CPA side, so auth failures back off more conservatively
// and are logged only when the state changes.
func watchCPA(ctx context.Context, client *cpa.Client, baseURL string, log *slog.Logger) {
	interval := pingBaseInterval
	healthy := false
	lastAuth := false

	for {
		pingCtx, cancel := context.WithTimeout(ctx, client.FlightTimeout())
		err := client.Ping(pingCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}

		switch {
		case err == nil:
			if !healthy {
				log.Info("cpa management api reachable", "api_version", client.APIVersion())
			}
			healthy = true
			lastAuth = false
			interval = pingBaseInterval
		case errors.Is(err, cpa.ErrUnauthorized):
			if !lastAuth {
				log.Error("cpa management authentication failed; backing off to avoid an IP ban", "err", err)
			}
			lastAuth = true
			healthy = false
			interval = pingAuthInterval
		default:
			if healthy || !lastAuth {
				log.Warn("cpa management api unreachable; running degraded", "err", err)
			}
			healthy = false
			interval = min(interval*2, pingMaxInterval)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// buildNotifiers assembles the outbound channels that are actually configured.
//
// The generic webhook is the channel-agnostic event layer; Feishu is the first
// concrete adapter. Neither is required for the watcher to run.
func buildNotifiers(cfg config.Config, refresher domain.QuotaRefresher, log *slog.Logger) ([]domain.Notifier, *feishu.Bot) {
	var notifiers []domain.Notifier

	if cfg.WebhookURL != "" {
		notifiers = append(notifiers, webhook.New(cfg.WebhookURL, nil))
		log.Info("webhook notifier enabled")
	}

	var bot *feishu.Bot
	if cfg.FeishuEnabled {
		b, err := feishu.New(cfg, refresher)
		if err != nil {
			// A misconfigured bot must not take the watcher down: the status
			// page and webhook still carry the signal.
			log.Error("feishu bot disabled", "err", err)
		} else {
			bot = b
			notifiers = append(notifiers, b)
			log.Info("feishu bot enabled", "chat_id", cfg.FeishuChatID)
		}
	}

	if len(notifiers) == 0 {
		log.Warn("no notification channel configured; alerts are visible only on the status page")
	}
	return notifiers, bot
}

func spawn(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}
