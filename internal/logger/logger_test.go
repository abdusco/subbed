package logger

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFromCtxReturnsTheLoggerToCtxStored(t *testing.T) {
	buffer := &bytes.Buffer{}
	l := slog.New(slog.NewTextHandler(buffer, nil))

	FromCtx(ToCtx(context.Background(), l)).Info("hello")

	assert.Contains(t, buffer.String(), "msg=hello")
}

func TestFromCtxFallsBackToTheDefaultLogger(t *testing.T) {
	assert.Same(t, slog.Default(), FromCtx(context.Background()))
}
