package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"subbed/internal/logger"
	"subbed/internal/subtitle"
)

const testSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nWorld\n"

// fakeTools stands in for yt-dlp and OpenRouter. Each hook has a working default.
type fakeTools struct {
	ready      error
	download   func(ctx context.Context, videoURL, cookiesFile string, progress progressFunc) (string, error)
	transcribe func(ctx context.Context, model, language string, progress progressFunc) (subtitle.Transcript, error)
	translate  func(ctx context.Context, cues []subtitle.Cue, language string, progress progressFunc) ([]subtitle.Cue, error)

	mutex           sync.Mutex
	downloads       []string
	cookiesSeen     string
	cookiesFile     string
	models          []string
	languages       []string
	translated      []string
	translateModels []string
}

func (f *fakeTools) Ready() error { return f.ready }

func (f *fakeTools) DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, progress progressFunc) (string, error) {
	f.mutex.Lock()
	f.downloads = append(f.downloads, videoURL)
	f.cookiesFile = cookiesFile
	if cookiesFile != "" {
		content, _ := os.ReadFile(cookiesFile)
		f.cookiesSeen = string(content)
	}
	f.mutex.Unlock()
	if f.download != nil {
		return f.download(ctx, videoURL, cookiesFile, progress)
	}
	logger.FromCtx(ctx).Debug("yt-dlp: [youtube] Downloading webpage")
	progress(1, "")
	return directory + "/abc.opus", nil
}

func (f *fakeTools) Transcribe(ctx context.Context, model, path, language string, progress progressFunc) (subtitle.Transcript, error) {
	f.mutex.Lock()
	f.models = append(f.models, model)
	f.languages = append(f.languages, language)
	f.mutex.Unlock()
	if f.transcribe != nil {
		return f.transcribe(ctx, model, language, progress)
	}
	logger.FromCtx(ctx).Debug("Transcribing chunk 1/1")
	progress(1, "Transcribing chunk 1/1")
	return subtitle.Transcript{SRT: testSRT, Language: "en", Cues: 2}, nil
}

func (f *fakeTools) Translate(ctx context.Context, model string, cues []subtitle.Cue, language string, progress progressFunc) ([]subtitle.Cue, error) {
	f.mutex.Lock()
	f.translated = append(f.translated, language)
	f.translateModels = append(f.translateModels, model)
	f.mutex.Unlock()
	if f.translate != nil {
		return f.translate(ctx, cues, language, progress)
	}
	progress(1, "")
	return prefixCues(cues, language+": "), nil
}

func prefixCues(cues []subtitle.Cue, prefix string) []subtitle.Cue {
	translated := make([]subtitle.Cue, len(cues))
	for position, cue := range cues {
		cue.Text = prefix + cue.Text
		translated[position] = cue
	}
	return translated
}

func newJobApp(t *testing.T, tools *fakeTools) (*testApp, *JobRunner) {
	t.Helper()
	var runner *JobRunner
	app := newTestAppFor(t, func(cfg *serverConfig) {
		runner = NewJobRunner(cfg.repo, tools, cfg.logger)
		cfg.jobs = runner
	})
	return app, runner
}

func (a *testApp) postJSON(path string, payload any, authed bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	return a.do(http.MethodPost, path, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"}, authed)
}

// addTestVideo adds a video and returns its id.
func addTestVideo(t *testing.T, app *testApp) int {
	t.Helper()
	rec := app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "Me at the zoo")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var added struct{ ID int }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &added))
	return added.ID
}

func transcribeRequest(app *testApp, videoID int, payload any) *httptest.ResponseRecorder {
	return app.postJSON(fmt.Sprintf("/api/admin/videos/%d/transcribe", videoID), payload, true)
}

func translateRequest(app *testApp, videoID int, payload any) *httptest.ResponseRecorder {
	return app.postJSON(fmt.Sprintf("/api/admin/videos/%d/translate", videoID), payload, true)
}

func decodeJob(t *testing.T, rec *httptest.ResponseRecorder) Job {
	t.Helper()
	var job Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job), rec.Body.String())
	return job
}

// storeEnglishSubtitle uploads an English subtitle the way the admin would.
func storeEnglishSubtitle(t *testing.T, app *testApp, videoID int, language string) {
	t.Helper()
	rec := app.upload(strconv.Itoa(videoID), language, "srt", "a.srt", testSRT)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func subtitleContents(t *testing.T, app *testApp, videoID int) map[string]string {
	t.Helper()
	subtitles, err := app.repo.GetSubtitlesByVideoID(context.Background(), videoID)
	require.NoError(t, err)
	contents := map[string]string{}
	for _, subtitle := range subtitles {
		contents[subtitle.Language] = subtitle.Content
	}
	return contents
}

func logOf(t *testing.T, app *testApp, jobID int64) string {
	t.Helper()
	logs, err := app.repo.ListJobLogs(context.Background(), jobID, 0, 1000)
	require.NoError(t, err)
	lines := make([]string, len(logs))
	for position, line := range logs {
		lines[position] = line.Level + " " + line.Message
	}
	return strings.Join(lines, "\n")
}

func runQueued(t *testing.T, runner *JobRunner) {
	t.Helper()
	require.NoError(t, runner.drain(context.Background()))
}

func TestTranscribeSavesTheSubtitleInTheDetectedLanguage(t *testing.T) {
	tools := &fakeTools{}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	cookies, err := parseNetscapeCookies(sampleCookies)
	require.NoError(t, err)
	require.NoError(t, app.repo.UpsertCookies(context.Background(), cookies))

	rec := transcribeRequest(app, videoID, map[string]any{})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	job := decodeJob(t, rec)
	assert.Equal(t, jobTranscribe, job.Kind)
	assert.Equal(t, jobQueued, job.Status)
	assert.Equal(t, "Me at the zoo", job.VideoTitle)
	assert.Empty(t, job.Languages)

	runQueued(t, runner)

	finished, err := app.repo.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, jobDone, finished.Status)
	assert.Empty(t, finished.Error)
	assert.Equal(t, map[string]string{"en": testSRT}, subtitleContents(t, app, videoID))
	assert.Empty(t, tools.translated, "a transcription does not translate")
	assert.Equal(t, []string{"https://www.youtube.com/watch?v=jNQXAC9IVRw"}, tools.downloads)
	assert.Equal(t, []string{"openai/whisper-large-v3-turbo"}, tools.models)
	assert.Equal(t, []string{""}, tools.languages)

	// yt-dlp got the stored cookies in a temporary file, which is gone afterwards
	assert.Contains(t, tools.cookiesSeen, "secret-sid")
	_, err = os.Stat(tools.cookiesFile)
	assert.True(t, os.IsNotExist(err), "the cookie file is removed")

	log := logOf(t, app, job.ID)
	for _, want := range []string{"info Started: Me at the zoo", "info yt-dlp: [youtube] Downloading webpage", "info Downloaded abc.opus",
		`The model detected the language "en"`, "Saved the en subtitle (2 cues)", "info Done"} {
		assert.Contains(t, log, want)
	}
}

func TestTranscribeUsesTheConfiguredModelAndSourceLanguage(t *testing.T) {
	tools := &fakeTools{transcribe: func(ctx context.Context, model, language string, progress progressFunc) (subtitle.Transcript, error) {
		return subtitle.Transcript{SRT: testSRT, Language: "ignored", Cues: 2}, nil
	}}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	require.NoError(t, app.repo.SetSetting(context.Background(), settingTranscribeModel, "openai/whisper-large-v3"))

	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{"source_language": "DE"}))
	assert.Equal(t, "de", job.SourceLanguage)
	runQueued(t, runner)

	assert.Equal(t, []string{"openai/whisper-large-v3"}, tools.models)
	assert.Equal(t, []string{"de"}, tools.languages)
	assert.Equal(t, map[string]string{"de": testSRT}, subtitleContents(t, app, videoID))
	assert.NotContains(t, logOf(t, app, job.ID), "detected", "the language was given")
}

func TestTranscribeReplacesAnExistingSubtitle(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	rec := app.upload(strconv.Itoa(videoID), "en", "srt", "a.srt", "1\n00:00:01,000 --> 00:00:02,000\nmanual\n")
	require.Equal(t, 200, rec.Code)

	transcribeRequest(app, videoID, map[string]any{})
	runQueued(t, runner)

	assert.Equal(t, map[string]string{"en": testSRT}, subtitleContents(t, app, videoID))
}

func TestTranscribeFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		tools *fakeTools
		want  string
	}{
		"download": {
			&fakeTools{download: func(context.Context, string, string, progressFunc) (string, error) {
				return "", errors.New("yt-dlp failed: Sign in to confirm you're not a bot")
			}},
			"Sign in to confirm",
		},
		"transcription": {
			&fakeTools{transcribe: func(context.Context, string, string, progressFunc) (subtitle.Transcript, error) {
				return subtitle.Transcript{}, errors.New("empty transcription")
			}},
			"transcription failed: empty transcription",
		},
		"language not detected": {
			&fakeTools{transcribe: func(context.Context, string, string, progressFunc) (subtitle.Transcript, error) {
				return subtitle.Transcript{SRT: testSRT, Language: "English (US)"}, nil
			}},
			"couldn't tell the spoken language",
		},
	} {
		t.Run(name, func(t *testing.T) {
			app, runner := newJobApp(t, tc.tools)
			videoID := addTestVideo(t, app)
			job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
			runQueued(t, runner)

			finished, err := app.repo.GetJob(context.Background(), job.ID)
			require.NoError(t, err)
			assert.Equal(t, jobFailed, finished.Status)
			assert.Contains(t, finished.Error, tc.want)
			assert.Empty(t, subtitleContents(t, app, videoID), "nothing is saved")
			assert.Contains(t, logOf(t, app, job.ID), "error Failed: ")
			assert.Equal(t, 202, transcribeRequest(app, videoID, map[string]any{}).Code, "a failed job does not block the next one")
		})
	}
}

func TestTranslateTranslatesAnExistingSubtitleWithoutDownloading(t *testing.T) {
	tools := &fakeTools{}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	storeEnglishSubtitle(t, app, videoID, "en")

	rec := translateRequest(app, videoID, map[string]any{"source_language": "EN", "languages": []string{"TR", " de ", "tr", "en"}})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	job := decodeJob(t, rec)
	assert.Equal(t, jobTranslate, job.Kind)
	assert.Equal(t, "en", job.SourceLanguage)
	assert.Equal(t, []string{"tr", "de"}, job.Languages, "duplicates and the source language are dropped")

	runQueued(t, runner)

	finished, err := app.repo.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, jobDone, finished.Status, finished.Error)
	contents := subtitleContents(t, app, videoID)
	require.Len(t, contents, 3)
	assert.Equal(t, testSRT, contents["en"], "the source is untouched")
	assert.Contains(t, contents["tr"], "Turkish: Hello")
	assert.Contains(t, contents["de"], "German: World")
	assert.Equal(t, []string{"Turkish", "German"}, tools.translated)
	assert.Empty(t, tools.downloads, "no audio is needed")
	assert.Empty(t, tools.models, "no transcription is needed")

	log := logOf(t, app, job.ID)
	for _, want := range []string{"Translating the en subtitle", "Translating 2 cues to Turkish", "Saved the tr subtitle", "info Done"} {
		assert.Contains(t, log, want)
	}
}

func TestTranslateAnyExistingLanguageIncludingOneThatWasGenerated(t *testing.T) {
	tools := &fakeTools{}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	transcribeRequest(app, videoID, map[string]any{})
	runQueued(t, runner)
	translateRequest(app, videoID, map[string]any{"source_language": "en", "languages": []string{"tr"}})
	runQueued(t, runner)

	// translate the Turkish one into German
	rec := translateRequest(app, videoID, map[string]any{"source_language": "tr", "languages": []string{"de"}})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	runQueued(t, runner)

	assert.Contains(t, subtitleContents(t, app, videoID)["de"], "German: Turkish: Hello")
}

func TestTranslateKeepsGoingWhenOneTranslationFails(t *testing.T) {
	tools := &fakeTools{}
	tools.translate = func(ctx context.Context, cues []subtitle.Cue, language string, progress progressFunc) ([]subtitle.Cue, error) {
		if language == "German" {
			return nil, errors.New("model refused")
		}
		return prefixCues(cues, language+": "), nil
	}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	storeEnglishSubtitle(t, app, videoID, "en")

	job := decodeJob(t, translateRequest(app, videoID, map[string]any{"source_language": "en", "languages": []string{"de", "tr", "fr"}}))
	runQueued(t, runner)

	finished, err := app.repo.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, jobFailed, finished.Status)
	assert.Equal(t, "translation failed for: de", finished.Error)
	contents := subtitleContents(t, app, videoID)
	assert.NotContains(t, contents, "de")
	assert.Contains(t, contents, "tr")
	assert.Contains(t, contents, "fr")
	assert.Contains(t, logOf(t, app, job.ID), "error Translating to de failed: model refused")
}

func TestTranslateFailsWhenTheSourceSubtitleIsGone(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	storeEnglishSubtitle(t, app, videoID, "en")
	job := decodeJob(t, translateRequest(app, videoID, map[string]any{"source_language": "en", "languages": []string{"tr"}}))

	subtitles, err := app.repo.GetSubtitlesByVideoID(context.Background(), videoID)
	require.NoError(t, err)
	require.NoError(t, app.repo.DeleteSubtitle(context.Background(), subtitles[0].ID))
	runQueued(t, runner)

	finished, err := app.repo.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, jobFailed, finished.Status)
	assert.Equal(t, "the video has no en subtitle to translate", finished.Error)
}

func TestJobRequestValidation(t *testing.T) {
	app, _ := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	storeEnglishSubtitle(t, app, videoID, "en")

	transcribePath := fmt.Sprintf("/api/admin/videos/%d/transcribe", videoID)
	translatePath := fmt.Sprintf("/api/admin/videos/%d/translate", videoID)
	for _, path := range []string{transcribePath, translatePath} {
		assert.Equal(t, 401, app.postJSON(path, map[string]any{}, false).Code, "%s without login", path)
	}

	tooMany := make([]string, maxTargetLanguages+1)
	for position := range tooMany {
		tooMany[position] = "tr"
	}
	for name, tc := range map[string]struct {
		path    string
		payload any
		status  int
		message string
	}{
		"transcribe: bad id":            {"/api/admin/videos/abc/transcribe", map[string]any{}, 400, "Invalid ID"},
		"transcribe: unknown video":     {"/api/admin/videos/999/transcribe", map[string]any{}, 404, "Video not found"},
		"transcribe: bad source":        {transcribePath, map[string]any{"source_language": "x"}, 400, "Invalid source language"},
		"translate: bad id":             {"/api/admin/videos/abc/translate", map[string]any{"source_language": "en", "languages": []string{"tr"}}, 400, "Invalid ID"},
		"translate: unknown video":      {"/api/admin/videos/999/translate", map[string]any{"source_language": "en", "languages": []string{"tr"}}, 404, "Video not found"},
		"translate: no source":          {translatePath, map[string]any{"languages": []string{"tr"}}, 400, "Invalid source language"},
		"translate: bad source":         {translatePath, map[string]any{"source_language": "english", "languages": []string{"tr"}}, 400, "Invalid source language"},
		"translate: source missing":     {translatePath, map[string]any{"source_language": "fr", "languages": []string{"tr"}}, 400, "The video has no subtitle in that language"},
		"translate: no targets":         {translatePath, map[string]any{"source_language": "en", "languages": []string{}}, 400, "Pick at least one language to translate into"},
		"translate: only the source":    {translatePath, map[string]any{"source_language": "en", "languages": []string{"en"}}, 400, "Pick at least one language to translate into"},
		"translate: bad language":       {translatePath, map[string]any{"source_language": "en", "languages": []string{"turkish!"}}, 400, "Invalid language code"},
		"translate: too many languages": {translatePath, map[string]any{"source_language": "en", "languages": tooMany}, 400, "Too many languages"},
		"translate: wrong json type":    {translatePath, map[string]any{"source_language": "en", "languages": "tr"}, 400, "Invalid request"},
	} {
		rec := app.postJSON(tc.path, tc.payload, true)
		assert.Equal(t, tc.status, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Equal(t, tc.message, errorMessage(t, rec), name)
	}

	jobs, err := app.repo.ListJobs(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, jobs, "rejected requests queue nothing")
}

func TestJobsAreRefusedWhileAnotherIsActiveOrNothingIsConfigured(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	storeEnglishSubtitle(t, app, videoID, "en")
	translation := map[string]any{"source_language": "en", "languages": []string{"tr"}}

	require.Equal(t, 202, transcribeRequest(app, videoID, map[string]any{}).Code)
	for _, rec := range []*httptest.ResponseRecorder{transcribeRequest(app, videoID, map[string]any{}), translateRequest(app, videoID, translation)} {
		assert.Equal(t, 409, rec.Code)
		assert.Contains(t, errorMessage(t, rec), "already")
	}
	runQueued(t, runner)
	assert.Equal(t, 202, translateRequest(app, videoID, translation).Code, "after the first job is over")

	unconfigured, _ := newJobApp(t, &fakeTools{ready: errors.New("OPENROUTER_API_KEY is not set")})
	id := addTestVideo(t, unconfigured)
	storeEnglishSubtitle(t, unconfigured, id, "en")
	for _, rec := range []*httptest.ResponseRecorder{transcribeRequest(unconfigured, id, map[string]any{}), translateRequest(unconfigured, id, translation)} {
		assert.Equal(t, 503, rec.Code)
		assert.Equal(t, "OPENROUTER_API_KEY is not set", errorMessage(t, rec))
	}
}

func TestJobsAndLogsEndpoints(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	first := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	runQueued(t, runner)
	second := decodeJob(t, translateRequest(app, videoID, map[string]any{"source_language": "en", "languages": []string{"tr"}}))

	assert.Equal(t, 401, app.do(http.MethodGet, "/api/admin/jobs", nil, nil, false).Code)
	var jobs []Job
	rec := app.do(http.MethodGet, "/api/admin/jobs", nil, nil, true)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &jobs), rec.Body.String())
	require.Len(t, jobs, 2)
	assert.Equal(t, second.ID, jobs[0].ID, "newest first")
	assert.Equal(t, jobTranslate, jobs[0].Kind)
	assert.Equal(t, jobQueued, jobs[0].Status)
	assert.Equal(t, first.ID, jobs[1].ID)
	assert.Equal(t, jobTranscribe, jobs[1].Kind)
	assert.Equal(t, jobDone, jobs[1].Status)
	assert.Equal(t, "Me at the zoo", jobs[1].VideoTitle)

	logsPath := fmt.Sprintf("/api/admin/jobs/%d/logs", first.ID)
	assert.Equal(t, 401, app.do(http.MethodGet, logsPath, nil, nil, false).Code)
	var all struct {
		Job  Job
		Logs []JobLog
	}
	rec = app.do(http.MethodGet, logsPath, nil, nil, true)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all), rec.Body.String())
	assert.Equal(t, jobDone, all.Job.Status)
	require.GreaterOrEqual(t, len(all.Logs), 5)
	assert.Equal(t, "Started: Me at the zoo", all.Logs[0].Message)
	assert.NotZero(t, all.Logs[0].At)
	assert.Equal(t, logInfo, all.Logs[0].Level)

	// polling with the id of the last line seen returns only what is new
	var rest struct{ Logs []JobLog }
	rec = app.do(http.MethodGet, fmt.Sprintf("%s?after=%d", logsPath, all.Logs[2].ID), nil, nil, true)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rest), rec.Body.String())
	require.Len(t, rest.Logs, len(all.Logs)-3)
	assert.Equal(t, all.Logs[3].ID, rest.Logs[0].ID)
	rec = app.do(http.MethodGet, fmt.Sprintf("%s?after=%d", logsPath, all.Logs[len(all.Logs)-1].ID), nil, nil, true)
	assert.Contains(t, rec.Body.String(), `"logs":[]`)

	for path, status := range map[string]int{
		"/api/admin/jobs/999/logs": 404,
		"/api/admin/jobs/abc/logs": 400,
		logsPath + "?after=x":      400,
		logsPath + "?after=-1":     400,
	} {
		assert.Equal(t, status, app.do(http.MethodGet, path, nil, nil, true).Code, path)
	}
}

// waitForStatus polls until the job has the status, so a test can wait for the background runner.
func waitForStatus(t *testing.T, app *testApp, id int64, status string) {
	t.Helper()
	require.Eventually(t, func() bool {
		job, err := app.repo.GetJob(context.Background(), id)
		return err == nil && job.Status == status
	}, 5*time.Second, 10*time.Millisecond, "job %d never became %s", id, status)
}

// progressOf reads a job's stored progress and stage.
func progressOf(t *testing.T, app *testApp, id int64) (int, string) {
	t.Helper()
	job, err := app.repo.GetJob(context.Background(), id)
	require.NoError(t, err)
	return job.Progress, job.Stage
}

func TestJobProgressMapsWorkOntoItsPartOfTheJobAndNeverGoesBack(t *testing.T) {
	ctx := context.Background()
	app, _ := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	id, err := app.repo.CreateJob(ctx, jobTranscribe, videoID, nil, "")
	require.NoError(t, err)
	progress := &jobProgress{repo: app.repo, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), jobID: id}

	half := progress.span(ctx, 0.2, 0.6, "Working")
	half(0.5, "")
	percent, stage := progressOf(t, app, id)
	assert.Equal(t, 40, percent, "half of the span from 20% to 60%")
	assert.Equal(t, "Working", stage, "the default stage applies")

	half(0.75, "Almost")
	percent, stage = progressOf(t, app, id)
	assert.Equal(t, 50, percent)
	assert.Equal(t, "Almost", stage, "the work's own stage wins")

	progress.set(ctx, 0.3, "")
	percent, stage = progressOf(t, app, id)
	assert.Equal(t, 50, percent, "progress never goes back")
	assert.Equal(t, "Almost", stage, "an empty stage keeps the last one")

	progress.set(ctx, 7, "Too far")
	percent, _ = progressOf(t, app, id)
	assert.Equal(t, 100, percent, "progress is capped at 100")
}

func TestTranscribeJobShowsProgressWhileItRuns(t *testing.T) {
	var app *testApp
	var seen [][2]any
	snapshot := func(t *testing.T, id int64) {
		percent, stage := progressOf(t, app, id)
		seen = append(seen, [2]any{percent, stage})
	}
	var jobID int64
	tools := &fakeTools{
		download: func(ctx context.Context, _, _ string, progress progressFunc) (string, error) {
			snapshot(t, jobID)
			progress(0.4, "")
			snapshot(t, jobID)
			progress(1, "")
			snapshot(t, jobID)
			return "/tmp/audio.opus", nil
		},
		transcribe: func(ctx context.Context, _, _ string, progress progressFunc) (subtitle.Transcript, error) {
			snapshot(t, jobID)
			progress(0.2, "Transcribing chunk 1/2")
			snapshot(t, jobID)
			return subtitle.Transcript{SRT: testSRT, Language: "en", Cues: 2}, nil
		},
	}
	var runner *JobRunner
	app, runner = newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	jobID = job.ID
	assert.Zero(t, job.Progress)
	assert.Empty(t, job.Stage)

	runQueued(t, runner)

	assert.Equal(t, [][2]any{
		{0, "Downloading audio"},
		{14, "Downloading audio"},      // 40% of the first 35%
		{35, "Downloading audio"},      // the download is done
		{35, "Transcribing"},           // the transcription starts
		{48, "Transcribing chunk 1/2"}, // 20% of the remaining 65%, on top of the 35%
	}, seen)
	percent, stage := progressOf(t, app, job.ID)
	assert.Equal(t, 100, percent, "a finished job is complete")
	assert.Empty(t, stage, "and not doing anything")
}

func TestTranslateJobSplitsProgressBetweenLanguages(t *testing.T) {
	var app *testApp
	var jobID int64
	var seen []int
	tools := &fakeTools{}
	tools.translate = func(ctx context.Context, cues []subtitle.Cue, language string, progress progressFunc) ([]subtitle.Cue, error) {
		progress(0.5, "")
		percent, stage := progressOf(t, app, jobID)
		seen = append(seen, percent)
		assert.Equal(t, "Translating to "+language, stage)
		return prefixCues(cues, language+": "), nil
	}
	var runner *JobRunner
	app, runner = newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	storeEnglishSubtitle(t, app, videoID, "en")
	jobID = decodeJob(t, translateRequest(app, videoID, map[string]any{"source_language": "en", "languages": []string{"tr", "de"}})).ID

	runQueued(t, runner)

	assert.Equal(t, []int{25, 75}, seen, "halfway through the first and the second of two languages")
}

func TestAFailedJobKeepsHowFarItGot(t *testing.T) {
	var app *testApp
	tools := &fakeTools{transcribe: func(context.Context, string, string, progressFunc) (subtitle.Transcript, error) {
		return subtitle.Transcript{}, errors.New("empty transcription")
	}}
	var runner *JobRunner
	app, runner = newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))

	runQueued(t, runner)

	percent, stage := progressOf(t, app, job.ID)
	assert.Equal(t, 35, percent, "the download was done")
	assert.Empty(t, stage)
}

// startRunner works through the queue in the background until the test ends.
func startRunner(t *testing.T, runner *JobRunner) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	runner.Start(ctx)
	t.Cleanup(func() {
		stop()
		runner.Wait()
	})
}

// blockingDownloads makes the first download wait until its job is canceled; later ones succeed. started receives a
// value when a download begins, and canceled when one was stopped.
func blockingDownloads() (tools *fakeTools, started, canceled chan struct{}) {
	started, canceled = make(chan struct{}, 8), make(chan struct{}, 8)
	var calls atomic.Int32
	tools = &fakeTools{download: func(ctx context.Context, _, _ string, _ progressFunc) (string, error) {
		started <- struct{}{}
		if calls.Add(1) > 1 {
			return "/tmp/audio.opus", nil
		}
		<-ctx.Done()
		canceled <- struct{}{}
		return "", ctx.Err()
	}}
	return tools, started, canceled
}

func cancelRequest(app *testApp, id int64) *httptest.ResponseRecorder {
	return app.do(http.MethodPost, fmt.Sprintf("/api/admin/jobs/%d/cancel", id), nil, nil, true)
}

func deleteRequest(app *testApp, id int64) *httptest.ResponseRecorder {
	return app.do(http.MethodDelete, fmt.Sprintf("/api/admin/jobs/%d", id), nil, nil, true)
}

func TestCancelAQueuedJob(t *testing.T) {
	tools := &fakeTools{}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))

	rec := cancelRequest(app, job.ID)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, jobCanceled, decodeJob(t, rec).Status)

	runQueued(t, runner)
	assert.Empty(t, tools.downloads, "a canceled job is never started")
	assert.Empty(t, subtitleContents(t, app, videoID))
	assert.Equal(t, 202, transcribeRequest(app, videoID, map[string]any{}).Code, "a canceled job doesn't block the video")
}

func TestCancelARunningJobStopsItAndTheQueueGoesOn(t *testing.T) {
	tools, started, canceled := blockingDownloads()
	app, runner := newJobApp(t, tools)
	startRunner(t, runner)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	<-started
	waitForStatus(t, app, job.ID, jobRunning)

	rec := cancelRequest(app, job.ID)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got := decodeJob(t, rec)
	assert.Equal(t, jobCanceled, got.Status, "the answer comes after the job has ended")
	assert.Empty(t, got.Error, "a cancellation is not an error")
	select {
	case <-canceled:
	default:
		t.Error("the download was not stopped")
	}
	log := logOf(t, app, job.ID)
	assert.Contains(t, log, "info Canceled")
	assert.NotContains(t, log, "error Failed")
	assert.Empty(t, subtitleContents(t, app, videoID))

	next := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	waitForStatus(t, app, next.ID, jobDone)
	assert.Contains(t, subtitleContents(t, app, videoID), "en", "the runner took the next job")
}

func TestCancelRefusals(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	runQueued(t, runner)

	assert.Equal(t, 401, app.do(http.MethodPost, fmt.Sprintf("/api/admin/jobs/%d/cancel", job.ID), nil, nil, false).Code, "without login")
	rec := cancelRequest(app, job.ID)
	assert.Equal(t, 409, rec.Code, "a finished job can't be canceled")
	assert.Equal(t, "The job is already over", errorMessage(t, rec))
	finished, err := app.repo.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, jobDone, finished.Status, "and stays as it was")

	assert.Equal(t, 404, cancelRequest(app, 999).Code)
	assert.Equal(t, 400, app.do(http.MethodPost, "/api/admin/jobs/abc/cancel", nil, nil, true).Code)
}

func TestDeleteAJobOfAnyKind(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	queued := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))

	assert.Equal(t, 401, app.do(http.MethodDelete, fmt.Sprintf("/api/admin/jobs/%d", queued.ID), nil, nil, false).Code, "without login")
	require.Equal(t, 200, deleteRequest(app, queued.ID).Code, "a queued job")
	runQueued(t, runner)
	assert.Empty(t, subtitleContents(t, app, videoID), "a deleted job is not run")

	done := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	runQueued(t, runner)
	require.Equal(t, 200, deleteRequest(app, done.ID).Code, "a finished job")
	assert.Contains(t, subtitleContents(t, app, videoID), "en", "its subtitle stays")

	jobs, err := app.repo.ListJobs(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	logs, err := app.repo.ListJobLogs(context.Background(), done.ID, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, logs, "the log goes with the job")

	assert.Equal(t, 404, deleteRequest(app, 999).Code)
	assert.Equal(t, 400, app.do(http.MethodDelete, "/api/admin/jobs/abc", nil, nil, true).Code)
}

func TestDeleteARunningJobStopsItFirst(t *testing.T) {
	tools, started, canceled := blockingDownloads()
	app, runner := newJobApp(t, tools)
	startRunner(t, runner)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	<-started
	waitForStatus(t, app, job.ID, jobRunning)

	require.Equal(t, 200, deleteRequest(app, job.ID).Code)

	select {
	case <-canceled:
	default:
		t.Error("the download was not stopped")
	}
	jobs, err := app.repo.ListJobs(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	logs, err := app.repo.ListJobLogs(context.Background(), job.ID, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, logs, "nothing is written for a job that was deleted")
	assert.Equal(t, 202, transcribeRequest(app, videoID, map[string]any{}).Code, "the video is free again")
}

func TestDeletingAVideoStopsItsRunningJob(t *testing.T) {
	tools, started, canceled := blockingDownloads()
	app, runner := newJobApp(t, tools)
	startRunner(t, runner)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	<-started
	waitForStatus(t, app, job.ID, jobRunning)

	require.Equal(t, 200, app.do(http.MethodDelete, fmt.Sprintf("/api/admin/videos/%d", videoID), nil, nil, true).Code)

	select {
	case <-canceled:
	default:
		t.Error("the job kept working for a video that is gone")
	}
	jobs, err := app.repo.ListJobs(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestInterruptedJobsAreFailedOnStart(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	ctx := context.Background()
	running, err := app.repo.CreateJob(ctx, jobTranscribe, videoID, nil, "")
	require.NoError(t, err)
	claimed, err := app.repo.ClaimNextJob(ctx)
	require.NoError(t, err)
	require.Equal(t, running, claimed.ID)
	queued, err := app.repo.CreateJob(ctx, jobTranscribe, videoID, nil, "")
	require.NoError(t, err)

	runCtx, stop := context.WithCancel(ctx)
	runner.Start(runCtx)
	waitForStatus(t, app, queued, jobDone)
	stop()
	runner.Wait()

	interrupted, err := app.repo.GetJob(ctx, running)
	require.NoError(t, err)
	assert.Equal(t, jobFailed, interrupted.Status)
	assert.Equal(t, "Interrupted by a restart", interrupted.Error)
}

func TestRunnerPicksUpJobsAddedWhileRunning(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	ctx, stop := context.WithCancel(context.Background())
	runner.Start(ctx)

	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	waitForStatus(t, app, job.ID, jobDone)
	stop()
	runner.Wait()
	assert.Contains(t, subtitleContents(t, app, videoID), "en")
}

func TestStoppingTheServerLeavesARunningJobForRecovery(t *testing.T) {
	started := make(chan struct{})
	tools := &fakeTools{download: func(ctx context.Context, _, _ string, _ progressFunc) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))

	ctx, stop := context.WithCancel(context.Background())
	runner.Start(ctx)
	<-started
	stop()
	runner.Wait()

	current, err := app.repo.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, jobRunning, current.Status, "left running, so the next start fails it as interrupted")
}

func TestDeletingAVideoDeletesItsJobs(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	job := decodeJob(t, transcribeRequest(app, videoID, map[string]any{}))
	runQueued(t, runner)

	require.Equal(t, 200, app.do(http.MethodDelete, fmt.Sprintf("/api/admin/videos/%d", videoID), nil, nil, true).Code)
	jobs, err := app.repo.ListJobs(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	logs, err := app.repo.ListJobLogs(context.Background(), job.ID, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, logs)
}

func TestUniqueLanguages(t *testing.T) {
	assert.Equal(t, []string{"tr", "de"}, uniqueLanguages([]string{"tr", "en", "tr", "de", "en"}, "en"))
}

func TestLanguageNames(t *testing.T) {
	assert.Equal(t, "Turkish", languageName("tr"))
	assert.Equal(t, "Brazilian Portuguese", languageName("pt-br"))
	assert.Contains(t, languageName("xx"), `"xx"`, "an unknown code is described by itself")

	for code, valid := range map[string]bool{"en": true, "pt-br": true, "eng": true, "e": false, "EN": false, "en_US": false, "english": false, "": false} {
		assert.Equal(t, valid, languageCodePattern.MatchString(code), "%q", code)
	}
}
