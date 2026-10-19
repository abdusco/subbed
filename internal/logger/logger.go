// Package logger carries a *slog.Logger in a context, so code that is handed a context logs with whatever the
// caller set up (the server, a request, a job) without being passed a logger.
package logger

import (
	"context"
	"log/slog"
)

type contextKey struct{}

// ToCtx returns a context that carries l.
func ToCtx(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// FromCtx returns the logger ctx carries, or the default logger if it carries none.
func FromCtx(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
