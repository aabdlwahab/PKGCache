// Package obs is the observability layer: structured logging, an in-process event
// bus, and metrics. Everything else depends on it; it depends on nothing internal.
package obs

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// Header and field names that must never reach a log line. The cache handles
// registry credentials, session cookies and CI write tokens; a single careless
// slog.Any of a header map would leak them all.
var redactedKeys = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-auth-token":        true,
	"password":            true,
	"token":               true,
	"secret":              true,
}

// LogOptions configures the process logger.
type LogOptions struct {
	Level  slog.Level
	Format string // "json" (default) or "text"
	Output io.Writer
}

// NewLogger builds the process logger. Values under a redacted key are replaced
// rather than dropped, so the shape of a log line never changes based on secrets.
func NewLogger(o LogOptions) *slog.Logger {
	if o.Output == nil {
		panic("obs: LogOptions.Output is required")
	}
	h := &slog.HandlerOptions{Level: o.Level, ReplaceAttr: redact}
	if strings.EqualFold(o.Format, "text") {
		return slog.New(slog.NewTextHandler(o.Output, h))
	}
	return slog.New(slog.NewJSONHandler(o.Output, h))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if redactedKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

// ctxKey is unexported so no other package can collide with our context slot.
type ctxKey struct{}

// WithLogger returns a context carrying lg, so request-scoped fields (project,
// ecosystem, request id) travel with the request instead of being re-derived.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, lg)
}

// LoggerFrom returns the context's logger, or slog.Default when there is none.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.Default()
}
