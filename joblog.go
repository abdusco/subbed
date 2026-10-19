package main

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"subbed/internal/logger"
)

const maxLogMessageRunes = 2000

// withJobLog returns a context whose logger writes to the server log, with the job's id, and to the job's own log
// in the database through jobLogSink, which the admin page shows. Debug lines (yt-dlp output, every batch of cues)
// are in the job's log but only show in the server log with DEBUG=1.
func withJobLog(ctx context.Context, repo *Repository, server *slog.Logger, jobID int64) context.Context {
	sink := &jobLogSink{repo: repo, server: server, jobID: jobID}
	return logger.ToCtx(ctx, slog.New(teeHandler{server.Handler(), sink}).With("job", jobID))
}

// jobLogSink is a slog.Handler that stores records in a job's log in the database. Attributes are not stored; the
// message is the line. A line that can't be saved is reported to the server log.
type jobLogSink struct {
	repo   *Repository
	server *slog.Logger
	jobID  int64
}

func (s *jobLogSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *jobLogSink) Handle(ctx context.Context, record slog.Record) error {
	level := logInfo
	if record.Level >= slog.LevelError {
		level = logError
	}
	message := record.Message
	if utf8.RuneCountInString(message) > maxLogMessageRunes {
		message = string([]rune(message)[:maxLogMessageRunes]) + "…"
	}
	// a canceled job can't write anymore, which is no cause for a warning
	if err := s.repo.AddJobLog(ctx, s.jobID, level, message); err != nil && ctx.Err() == nil {
		s.server.Warn("Couldn't save a job log line", "job", s.jobID, "error", err)
	}
	return nil
}

func (s *jobLogSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *jobLogSink) WithGroup(string) slog.Handler      { return s }

// teeHandler sends every record to the server's handler and to the job's sink, each of which decides for itself
// which levels it takes.
type teeHandler struct {
	server slog.Handler
	sink   slog.Handler
}

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.server.Enabled(ctx, level) || t.sink.Enabled(ctx, level)
}

func (t teeHandler) Handle(ctx context.Context, record slog.Record) error {
	if t.server.Enabled(ctx, record.Level) {
		_ = t.server.Handle(ctx, record.Clone())
	}
	return t.sink.Handle(ctx, record)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{t.server.WithAttrs(attrs), t.sink.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{t.server.WithGroup(name), t.sink.WithGroup(name)}
}
