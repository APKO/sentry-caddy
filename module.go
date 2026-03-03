package sentrycaddy

import (
	"fmt"
	"net/http"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(new(SentryHandler))
	httpcaddyfile.RegisterHandlerDirective("sentry", parseCaddyfile)
}

type SentryHandler struct {
	DSN           string `json:"dsn,omitempty"`
	Environment   string `json:"environment,omitempty"`
	Release       string `json:"release,omitempty"`
	EnableTracing bool   `json:"enable_tracing,omitempty"`
	Name          string `json:"name,omitempty"`

	client *sentry.Client
	logger *zap.Logger
}

func (SentryHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sentry",
		New: func() caddy.Module { return new(SentryHandler) },
	}
}

func (h *SentryHandler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger().With(zap.String("dsn", h.DSN))

	if h.DSN == "" {
		h.logger.Error("Sentry DSN is empty")
		return fmt.Errorf("sentry: dsn is required")
	}

	opts := sentry.ClientOptions{
		Dsn:              h.DSN,
		Environment:      h.Environment,
		Release:          h.Release,
		AttachStacktrace: true,
		SendDefaultPII:   true,
	}

	if h.EnableTracing {
		opts.EnableTracing = true
		opts.TracesSampleRate = 1.0
	}

	var err error
	h.client, err = sentry.NewClient(opts)
	if err != nil {
		h.logger.Error("Failed to create Sentry client", zap.Error(err))
		return fmt.Errorf("sentry client init failed: %w", err)
	}

	h.logger.Info("Sentry client successfully created",
		zap.String("environment", h.Environment),
		zap.Bool("tracing", h.EnableTracing),
	)
	return nil
}

func (h SentryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.client == nil {
		h.logger.Warn("No Sentry client available — skipping Sentry")
		return next.ServeHTTP(w, r)
	}

	localHub := sentry.NewHub(h.client, sentry.NewScope())

	localHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetRequest(r)
		scope.SetUser(sentry.User{
			Username:  h.Name,
			IPAddress: r.RemoteAddr,
		})
	})

	ctx := sentry.SetHubOnContext(r.Context(), localHub)
	r = r.WithContext(ctx)

	sentryHandler := sentryhttp.New(sentryhttp.Options{
		Repanic:         true,
		WaitForDelivery: false,
		Timeout:         0,
	})

	wrapped := sentryHandler.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := sentry.SpanFromContext(r.Context())
		if span != nil {
			// Пропагація заголовків (використовуємо константи з sentry-go)
			r.Header.Set(sentry.SentryTraceHeader, span.ToSentryTrace())
			r.Header.Set(sentry.SentryBaggageHeader, span.ToBaggage())

			// W3C traceparent — генеруємо вручну
			traceID := span.TraceID.String()
			spanID := span.SpanID.String()
			sampled := "00"
			if span.Sampled.Bool() {
				sampled = "01"
			}
			traceparent := fmt.Sprintf("00-%s-%s-%s", traceID, spanID, sampled)
			r.Header.Set(sentry.TraceparentHeader, traceparent)

			h.logger.Debug("Propagating tracing headers",
				zap.String(sentry.SentryTraceHeader, span.ToSentryTrace()),
				zap.String(sentry.SentryBaggageHeader, span.ToBaggage()),
				zap.String(sentry.TraceparentHeader, traceparent),
			)
		}

		if err := next.ServeHTTP(w, r); err != nil {
			sentry.CaptureException(err)
		}
	}))

	wrapped.ServeHTTP(w, r)
	return nil
}

func (h *SentryHandler) Validate() error {
	if h.DSN == "" {
		return fmt.Errorf("sentry: dsn обов'язковий")
	}
	if h.Name == "" {
		return fmt.Errorf("sentry: name обов'язковий")
	}
	return nil
}

func (h *SentryHandler) Cleanup() error {
	if h.client != nil {
		flushed := h.client.Flush(2 * time.Second)
		if !flushed {
			h.logger.Warn("Sentry flush timed out — some events may be lost")
		} else {
			h.logger.Info("Sentry flush completed")
		}
	}
	h.logger.Info("Sentry handler cleaned up")
	return nil
}

func (h *SentryHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() {
		return d.Err("очікується директива 'sentry'")
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "dsn":
			if !d.Args(&h.DSN) {
				return d.ArgErr()
			}
			if d.NextArg() {
				return d.Err("dsn приймає тільки один аргумент")
			}
		case "environment":
			if !d.Args(&h.Environment) {
				return d.ArgErr()
			}
		case "release":
			if !d.Args(&h.Release) {
				return d.ArgErr()
			}
		case "name":
			if !d.Args(&h.Name) {
				return d.ArgErr()
			}
		case "tracing":
			h.EnableTracing = true
			if d.NextArg() {
				return d.Err("tracing не приймає аргументів")
			}
		default:
			return d.Errf("невідома опція в sentry: %s", d.Val())
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m SentryHandler
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

var (
	_ caddy.Provisioner           = (*SentryHandler)(nil)
	_ caddy.Validator             = (*SentryHandler)(nil)
	_ caddy.CleanerUpper          = (*SentryHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*SentryHandler)(nil)
	_ caddyfile.Unmarshaler       = (*SentryHandler)(nil)
)
