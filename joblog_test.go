package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"subbed/internal/logger"
)

func TestJobLogGoesToTheDatabaseAndTheServerLog(t *testing.T) {
	app, _ := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	id, err := app.repo.CreateJob(context.Background(), jobTranscribe, videoID, nil, "")
	require.NoError(t, err)

	server := &bytes.Buffer{}
	ctx := withJobLog(context.Background(), app.repo, slog.New(slog.NewTextHandler(server, nil)), id)
	log := logger.FromCtx(ctx)
	log.Info("Downloading audio")
	log.Debug("yt-dlp: 50%")
	log.Error("Failed: boom")

	lines, err := app.repo.ListJobLogs(context.Background(), id, 0, 10)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"Downloading audio", "yt-dlp: 50%", "Failed: boom"},
		[]string{lines[0].Message, lines[1].Message, lines[2].Message})
	assert.Equal(t, []string{logInfo, logInfo, logError}, []string{lines[0].Level, lines[1].Level, lines[2].Level})

	// the server log has the job id, and leaves out the debug line the job log keeps
	assert.Contains(t, server.String(), `msg="Downloading audio" job=`)
	assert.Contains(t, server.String(), "level=ERROR")
	assert.NotContains(t, server.String(), "yt-dlp")
	assert.Equal(t, 2, bytes.Count(server.Bytes(), []byte("\n")))
}
