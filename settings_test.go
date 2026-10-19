package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslationModelSettings(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	assert.Equal(t, 401, app.putJSON("/api/admin/settings", map[string]string{"translate_model": "provider/custom"}, false).Code)
	for _, tc := range []struct {
		name     string
		body     map[string]string
		status   int
		stored   string
		resolved string
	}{
		{"default", map[string]string{}, 200, "", "google/gemini-3.5-flash-lite"},
		{"save and trim", map[string]string{"translate_model": "  provider/custom  "}, 200, "provider/custom", "provider/custom"},
		{"omitted", map[string]string{"transcribe_model": "provider/speech"}, 200, "provider/custom", "provider/custom"},
		{"space", map[string]string{"translate_model": "two words"}, 400, "provider/custom", "provider/custom"},
		{"newline", map[string]string{"translate_model": "a\nb"}, 400, "provider/custom", "provider/custom"},
		{"too long", map[string]string{"translate_model": strings.Repeat("m", maxModelLen+1)}, 400, "provider/custom", "provider/custom"},
		{"reset", map[string]string{"translate_model": " "}, 200, "", "google/gemini-3.5-flash-lite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := app.putJSON("/api/admin/settings", tc.body, true)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			rec = app.do(http.MethodGet, "/api/admin/settings", nil, nil, true)
			require.Equal(t, 200, rec.Code)
			var settings settingsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
			assert.Equal(t, tc.stored, settings.TranslateModel)
			assert.Equal(t, "google/gemini-3.5-flash-lite", settings.TranslateModelDefault)
			model, err := app.repo.translateModel(ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.resolved, model)
		})
	}
	// Validate both fields before changing either setting.
	rec := app.putJSON("/api/admin/settings", map[string]string{
		"transcribe_model": "provider/changed", "translate_model": "invalid model",
	}, true)
	require.Equal(t, 400, rec.Code)
	model, err := app.repo.transcribeModel(ctx)
	require.NoError(t, err)
	assert.Equal(t, "provider/speech", model)
}

func TestTranslationJobUsesConfiguredModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved string
		want  string
	}{
		{"built-in default", "", "google/gemini-3.5-flash-lite"},
		{"saved override", "provider/custom", "provider/custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := &fakeTools{}
			app, runner := newJobApp(t, tools)
			videoID := addTestVideo(t, app)
			storeEnglishSubtitle(t, app, videoID, "en")
			rec := translateRequest(app, videoID, map[string]any{"source_language": "en", "languages": []string{"tr"}})
			require.Equal(t, 202, rec.Code, rec.Body.String())
			job := decodeJob(t, rec)
			// A choice saved after queuing still applies when the job runs.
			rec = app.putJSON("/api/admin/settings", map[string]string{"translate_model": tc.saved}, true)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			runQueued(t, runner)
			finished, err := app.repo.GetJob(context.Background(), job.ID)
			require.NoError(t, err)
			require.Equal(t, jobDone, finished.Status, finished.Error)
			assert.Equal(t, []string{tc.want}, tools.translateModels)
			assert.Contains(t, logOf(t, app, job.ID), "Translating with "+tc.want)
		})
	}
}
