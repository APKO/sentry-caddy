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

// SentryHandler — це middleware для Caddy, що обгортає Sentry HTTP handler
type SentryHandler struct {
	// Опціонально: можна додати налаштування з Caddyfile/JSON
	DSN           string `json:"dsn,omitempty"`
	Environment   string `json:"environment,omitempty"`
	Release       string `json:"release,omitempty"`
	EnableTracing bool   `json:"enable_tracing,omitempty"`
	logger        *zap.Logger
}

// CaddyModule — реєстрація модуля
func (SentryHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sentry",
		New: func() caddy.Module { return new(SentryHandler) },
	}
}

// Provision — ініціалізація (виконується один раз при старті)
func (h *SentryHandler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	h.logger.Info("Sentry handler provisioned",
		zap.String("dsn", h.DSN),
		zap.String("environment", h.Environment),
		zap.Bool("tracing", h.EnableTracing),
	)
	if h.DSN == "" {
		h.logger.Error("Sentry DSN is empty — Sentry will not be initialized")
		return fmt.Errorf("sentry: dsn is required")
	}

	opts := sentry.ClientOptions{
		Dsn:              h.DSN,
		Environment:      h.Environment,
		Release:          h.Release,
		EnableTracing:    true,
		AttachStacktrace: true,
		SendDefaultPII:   true,
	}

	// Якщо хочеш tracing (performance)
	if h.EnableTracing {
		opts.TracesSampleRate = 1.0 // або 0.2, 0.01 тощо
	}

	err := sentry.Init(opts)
	if err != nil {
		h.logger.Error("Failed to initialize Sentry", zap.Error(err))
		return fmt.Errorf("sentry init failed: %w", err)
	}
	h.logger.Info("Sentry successfully initialized")
	return nil
}

// ServeHTTP — саме те, що потрібно: обгортаємо наступний handler
func (h SentryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	h.logger.Debug("Handling request with Sentry",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("remote_ip", r.RemoteAddr),
	)
	sentryHandler := sentryhttp.New(sentryhttp.Options{
		// Тут можна налаштувати, що репортити
		Repanic:         true,
		WaitForDelivery: false,
		Timeout:         0,
	})

	wrapped := sentryHandler.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Отримуємо поточний Hub (він вже встановлений sentryhttp.Handler)
		hub := sentry.GetHubFromContext(r.Context())
		if hub == nil {
			hub = sentry.CurrentHub()
		}

		// Витягуємо trace та baggage для передачі далі
		sentryTrace := hub.GetTraceparent() // або hub.Client().GetTraceparent(), якщо потрібно
		baggage := hub.GetBaggage()

		// Додаємо їх до запиту (щоб reverse_proxy міг їх скопіювати)
		if sentryTrace != "" {
			r.Header.Set(sentry.SentryTraceHeader, sentryTrace)
		}
		if baggage != "" {
			r.Header.Set(sentry.SentryBaggageHeader, baggage)
		}

		// Якщо хочеш бачити в логах, що передаємо
		h.logger.Debug("Propagating Sentry trace",
			zap.String(sentry.SentryTraceHeader, sentryTrace),
			zap.String(sentry.SentryBaggageHeader, baggage),
		)

		// Викликаємо наступний handler (reverse_proxy побачить оновлені заголовки)
		err := next.ServeHTTP(w, r)
		if err != nil {
			sentry.CaptureException(err)
		}
	}))

	wrapped.ServeHTTP(w, r)
	return nil
}

// Validate — опціонально перевіряємо конфігурацію
func (h *SentryHandler) Validate() error {
	if h.DSN == "" {
		return fmt.Errorf("sentry: dsn обов'язковий")
	}
	return nil
}

func (h *SentryHandler) Cleanup() error {
	flushed := sentry.Flush(2 * time.Second)
	if !flushed {
		h.logger.Warn("Sentry flush timed out — some events may be lost")
	} else {
		h.logger.Info("Sentry flush completed successfully")
	}

	h.logger.Info("Sentry handler cleaned up")
	return nil
}

func (h *SentryHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Спочатку споживаємо ім'я директиви "sentry"
	if !d.Next() {
		return d.Err("очікується директива 'sentry'")
	}

	// Тепер парсимо блок { ... }
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

		case "tracing":
			h.EnableTracing = true
			// Не приймає аргументи
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
