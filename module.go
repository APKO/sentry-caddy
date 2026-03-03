package sentrycaddy

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
	h.logger = ctx.Logger().With(zap.String("name", h.Name))

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
		zap.String("name", h.Name),
	)
	return nil
}

func (h SentryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.client == nil {
		h.logger.Warn("No Sentry client available — skipping Sentry")
		return next.ServeHTTP(w, r)
	}

	localHub := sentry.NewHub(h.client, sentry.NewScope())
	ctx := sentry.SetHubOnContext(r.Context(), localHub)

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

	tx := sentry.StartTransaction(ctx, getHTTPSpanName(r), spanOpts...)
	tx.SetData("http.request.method", r.Method)
	tx.SetData("http.request.host", r.Host)

	ctx = tx.Context()
	r = r.WithContext(ctx)

	rw := wrapResponseWriter(w, r.ProtoMajor)

	localHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetRequest(r)
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}
		scope.SetUser(sentry.User{
			Username:  h.Name,
			IPAddress: clientIP,
		})
	})

	// === ДОДАНО: передача tracing-заголовків на downstream (reverse_proxy тощо) ===
	if tx != nil && h.EnableTracing {
		r.Header.Set(sentry.SentryTraceHeader, tx.ToSentryTrace())
		r.Header.Set(sentry.SentryBaggageHeader, tx.ToBaggage())

		// W3C traceparent (для максимальної сумісності з іншими системами)
		traceparent := fmt.Sprintf("00-%s-%s-01", tx.TraceID.String(), tx.SpanID.String())
		r.Header.Set(sentry.TraceparentHeader, traceparent)

		h.logger.Debug("Propagating tracing headers to downstream",
			zap.String(sentry.SentryTraceHeader, tx.ToSentryTrace()),
			zap.String(sentry.SentryBaggageHeader, tx.ToBaggage()),
			zap.String(sentry.TraceparentHeader, traceparent),
		)
	}

	defer func() {
		if tx != nil {
			status := rw.Status()
			tx.Status = sentry.HTTPtoSpanStatus(status)
			tx.SetData("http.response.status_code", status)
			tx.Finish()
		}

		if rec := recover(); rec != nil {
			eventID := localHub.RecoverWithContext(
				context.WithValue(r.Context(), sentry.RequestContextKey, r),
				rec,
			)
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

func getHTTPSpanName(r *http.Request) string {
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	return fmt.Sprintf("%s %s", r.Method, path)
}

type responseWrapper interface {
	http.ResponseWriter
	Status() int
}

type basicResponseWriter struct {
	http.ResponseWriter
	status int
}

func (b *basicResponseWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
	b.ResponseWriter.WriteHeader(code)
}

func (b *basicResponseWriter) Status() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

type flushResponseWriter struct {
	basicResponseWriter
}

func (f *flushResponseWriter) Flush() {
	f.ResponseWriter.(http.Flusher).Flush()
}

func wrapResponseWriter(w http.ResponseWriter, protoMajor int) responseWrapper {
	b := &basicResponseWriter{ResponseWriter: w}
	if _, ok := w.(http.Flusher); ok {
		return &flushResponseWriter{basicResponseWriter: *b}
	}
	return b
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
