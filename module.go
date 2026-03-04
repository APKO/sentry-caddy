package sentrycaddy

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/getsentry/sentry-go"
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
	h.logger = ctx.Logger().With(zap.String("handler_name", h.Name))

	if h.DSN == "" || h.Name == "" {
		return fmt.Errorf("sentry: dsn та name обов'язкові")
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
		zap.String("name", h.Name),
	)
	return nil
}

func (h SentryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.client == nil {
		h.logger.Warn("No Sentry client available — skipping Sentry")
		return next.ServeHTTP(w, r)
	}

	// Спроба взяти існуючий hub — найчастіший кейс
	localHub := sentry.GetHubFromContext(r.Context())
	if localHub == nil {
		localHub = sentry.NewHub(h.client, sentry.NewScope())
		ctx := sentry.SetHubOnContext(r.Context(), localHub)
		r = r.WithContext(ctx)
	}

	var tx *sentry.Span
	if h.EnableTracing {
		opName := "http.server"
		if r.Host != "" {
			opName += " " + r.Host
		}

		spanOpts := []sentry.SpanOption{
			sentry.ContinueTrace(localHub, r.Header.Get(sentry.SentryTraceHeader), r.Header.Get(sentry.SentryBaggageHeader)),
			sentry.WithOpName(opName),
			sentry.WithTransactionSource(sentry.SourceURL),
			sentry.WithSpanOrigin("auto.http.caddy"),
		}

		tx = sentry.StartTransaction(r.Context(), getSpanName(r), spanOpts...)
		tx.SetData("http.method", r.Method)
		tx.SetData("http.host", r.Host)

		r = r.WithContext(tx.Context())
	}

	rw := &statusCapturer{ResponseWriter: w}

	localHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetRequest(r)
		scope.SetUser(sentry.User{
			Username:  h.Name,
			IPAddress: realIP(r),
		})
		scope.SetTag("handler.name", h.Name) // зручно фільтрувати в Sentry
	})

	// Передача tracing-заголовків у upstream-запити
	if tx != nil && h.EnableTracing {
		r.Header.Set(sentry.SentryTraceHeader, tx.ToSentryTrace())
		r.Header.Set(sentry.SentryBaggageHeader, tx.ToBaggage())

		// W3C traceparent — найпоширеніший стандарт
		traceparent := "00-" + tx.TraceID.String() + "-" + tx.SpanID.String() + "-01"
		r.Header.Set(sentry.TraceparentHeader, traceparent)
	}

	defer func() {
		if tx != nil {
			tx.Status = sentry.HTTPtoSpanStatus(rw.status)
			tx.SetData("http.response.status_code", rw.status)
			tx.Finish()
		}

		if rec := recover(); rec != nil {
			eventID := localHub.RecoverWithContext(r.Context(), rec)
			if eventID != nil {
				localHub.Flush(10 * time.Second)
			}
			panic(rec)
		}
	}()

	err := next.ServeHTTP(rw, r)
	if err != nil {
		localHub.CaptureException(err)
	}

	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Швидкі допоміжні функції без fmt.Sprintf
// ─────────────────────────────────────────────────────────────────────────────

func getSpanName(r *http.Request) string {
	path := r.URL.Path
	return r.Method + " " + path
}

type statusCapturer struct {
	http.ResponseWriter
	status int
}

func (sc *statusCapturer) WriteHeader(code int) {
	if sc.status == 0 {
		sc.status = code
	}
	sc.ResponseWriter.WriteHeader(code)
}

func (sc *statusCapturer) Status() int {
	if sc.status == 0 {
		return http.StatusOK
	}
	return sc.status
}

// ─────────────────────────────────────────────────────────────────────────────
// Caddy-методи
// ─────────────────────────────────────────────────────────────────────────────

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
		if !h.client.Flush(2 * time.Second) {
			h.logger.Warn("Sentry flush таймаут")
		} else {
			h.logger.Info("Sentry flush OK")
		}
	}
	h.logger.Info("Sentry handler cleaned up")
	return nil
}

func (h *SentryHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "dsn":
				if !d.Args(&h.DSN) {
					return d.ArgErr()
				}
			case "environment":
				d.Args(&h.Environment)
			case "release":
				d.Args(&h.Release)
			case "name":
				if !d.Args(&h.Name) {
					return d.ArgErr()
				}
			case "tracing":
				h.EnableTracing = true
			default:
				return d.Errf("невідома опція в sentry: %s", d.Val())
			}
		}
	}
	return nil
}

var trueClientIP = http.CanonicalHeaderKey("True-Client-IP")
var xForwardedFor = http.CanonicalHeaderKey("X-Forwarded-For")
var xRealIP = http.CanonicalHeaderKey("X-Real-IP")

func realIP(r *http.Request) string {
	var ip string

	if tcip := r.Header.Get(trueClientIP); tcip != "" {
		ip = tcip
	} else if xrip := r.Header.Get(xRealIP); xrip != "" {
		ip = xrip
	} else if xff := r.Header.Get(xForwardedFor); xff != "" {
		ip, _, _ = strings.Cut(xff, ",")
	}
	if ip == "" || net.ParseIP(ip) == nil {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}
		return clientIP
	}
	return ip
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var s SentryHandler
	return &s, s.UnmarshalCaddyfile(h.Dispenser)
}

var (
	_ caddy.Provisioner           = (*SentryHandler)(nil)
	_ caddy.Validator             = (*SentryHandler)(nil)
	_ caddy.CleanerUpper          = (*SentryHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*SentryHandler)(nil)
	_ caddyfile.Unmarshaler       = (*SentryHandler)(nil)
)
