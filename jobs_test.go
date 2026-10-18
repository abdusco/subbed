package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"subbed/internal/subtitle"
)

const testSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nWorld\n"

// fakeTools stands in for yt-dlp and OpenRouter. Each hook has a working default.
type fakeTools struct {
	ready      error
	download   func(ctx context.Context, videoURL, cookiesFile string, logf logFunc) (string, error)
	transcribe func(ctx context.Context, model, language string) (subtitle.Transcript, error)
	translate  func(ctx context.Context, cues []subtitle.Cue, language string) ([]subtitle.Cue, error)

	mutex       sync.Mutex
	downloads   []string
	cookiesSeen string
	cookiesFile string
	models      []string
	languages   []string
	translated  []string
}

func (f *fakeTools) Ready() error { return f.ready }

func (f *fakeTools) DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, logf logFunc) (string, error) {
	f.mutex.Lock()
	f.downloads = append(f.downloads, videoURL)
	f.cookiesFile = cookiesFile
	if cookiesFile != "" {
		content, _ := os.ReadFile(cookiesFile)
		f.cookiesSeen = string(content)
	}
	f.mutex.Unlock()
	if f.download != nil {
		return f.download(ctx, videoURL, cookiesFile, logf)
	}
	logf("[youtube] Downloading webpage")
	return directory + "/abc.webm", nil
}

func (f *fakeTools) Transcribe(ctx context.Context, model, path, language string, logf logFunc) (subtitle.Transcript, error) {
	f.mutex.Lock()
	f.models = append(f.models, model)
	f.languages = append(f.languages, language)
	f.mutex.Unlock()
	if f.transcribe != nil {
		return f.transcribe(ctx, model, language)
	}
	logf("Transcribing chunk 1/1")
	return subtitle.Transcript{SRT: testSRT, Language: "en", Cues: 2}, nil
}

func (f *fakeTools) Translate(ctx context.Context, cues []subtitle.Cue, language string, logf logFunc) ([]subtitle.Cue, error) {
	f.mutex.Lock()
	f.translated = append(f.translated, language)
	f.mutex.Unlock()
	if f.translate != nil {
		return f.translate(ctx, cues, language)
	}
	translated := make([]subtitle.Cue, len(cues))
	for position, cue := range cues {
		cue.Text = language + ": " + cue.Text
		translated[position] = cue
	}
	return translated, nil
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
	var added struct{ ID int }
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil || rec.Code != 200 {
		t.Fatalf("add video: %d %q", rec.Code, rec.Body.String())
	}
	return added.ID
}

func generate(app *testApp, videoID int, payload any) *httptest.ResponseRecorder {
	return app.postJSON(fmt.Sprintf("/api/admin/videos/%d/generate", videoID), payload, true)
}

func decodeJob(t *testing.T, rec *httptest.ResponseRecorder) Job {
	t.Helper()
	var job Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("not a job: %v %q", err, rec.Body.String())
	}
	return job
}

func subtitleContents(t *testing.T, app *testApp, videoID int) map[string]string {
	t.Helper()
	subtitles, err := app.repo.GetSubtitlesByVideoID(context.Background(), videoID)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, subtitle := range subtitles {
		contents[subtitle.Language] = subtitle.Content
	}
	return contents
}

func jobLogLines(t *testing.T, app *testApp, jobID int64) []string {
	t.Helper()
	logs, err := app.repo.ListJobLogs(context.Background(), jobID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, len(logs))
	for position, line := range logs {
		lines[position] = line.Level + " " + line.Message
	}
	return lines
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func TestGenerateTranscribesAndTranslates(t *testing.T) {
	tools := &fakeTools{}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	cookies, _ := parseNetscapeCookies(sampleCookies)
	if err := app.repo.UpsertCookies(context.Background(), cookies); err != nil {
		t.Fatal(err)
	}

	rec := generate(app, videoID, map[string]any{"languages": []string{"TR", " de ", "tr", "en"}})
	if rec.Code != 202 {
		t.Fatalf("generate: %d %q", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec)
	if job.Status != jobQueued || job.VideoTitle != "Me at the zoo" || strings.Join(job.Languages, ",") != "tr,de,en" {
		t.Fatalf("job = %+v", job)
	}

	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	finished, err := app.repo.GetJob(context.Background(), job.ID)
	if err != nil || finished.Status != jobDone || finished.Error != "" {
		t.Fatalf("job = %+v, %v", finished, err)
	}
	contents := subtitleContents(t, app, videoID)
	if len(contents) != 3 || contents["en"] != testSRT {
		t.Fatalf("subtitles = %v", contents)
	}
	if !strings.Contains(contents["tr"], "Turkish: Hello") || !strings.Contains(contents["de"], "German: World") {
		t.Errorf("translations = %q", contents)
	}
	// the detected source language is not translated into itself
	if strings.Join(tools.translated, ",") != "Turkish,German" {
		t.Errorf("translated into %v", tools.translated)
	}
	if tools.downloads[0] != "https://www.youtube.com/watch?v=jNQXAC9IVRw" || tools.models[0] != "elevenlabs/scribe-v2" || tools.languages[0] != "" {
		t.Errorf("downloads %v, models %v, languages %v", tools.downloads, tools.models, tools.languages)
	}

	// yt-dlp got the stored cookies in a temporary file, which is gone afterwards
	if !strings.Contains(tools.cookiesSeen, "secret-sid") {
		t.Errorf("cookie file content = %q", tools.cookiesSeen)
	}
	if _, err := os.Stat(tools.cookiesFile); !os.IsNotExist(err) {
		t.Errorf("cookie file left behind: %v", err)
	}

	lines := jobLogLines(t, app, job.ID)
	for _, want := range []string{"info Started: Me at the zoo", "info yt-dlp: [youtube] Downloading webpage", "info Downloaded abc.webm",
		`The model detected the language "en"`, "Saved the en subtitle (2 cues)", "Translating 2 cues to Turkish", "Saved the tr subtitle", "info Done"} {
		if !containsLine(lines, want) {
			t.Errorf("log lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

func TestGenerateUsesTheConfiguredModelAndSourceLanguage(t *testing.T) {
	tools := &fakeTools{transcribe: func(ctx context.Context, model, language string) (subtitle.Transcript, error) {
		return subtitle.Transcript{SRT: testSRT, Language: "ignored", Cues: 2}, nil
	}}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	if err := app.repo.SetSetting(context.Background(), settingTranscribeModel, "openai/whisper-large-v3"); err != nil {
		t.Fatal(err)
	}

	rec := generate(app, videoID, map[string]any{"languages": []string{"de", "tr"}, "source_language": "DE"})
	job := decodeJob(t, rec)
	if job.SourceLanguage != "de" || strings.Join(job.Languages, ",") != "tr" {
		t.Fatalf("job = %+v", job)
	}
	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	if tools.models[0] != "openai/whisper-large-v3" || tools.languages[0] != "de" {
		t.Errorf("models %v, languages %v", tools.models, tools.languages)
	}
	contents := subtitleContents(t, app, videoID)
	if _, found := contents["ignored"]; found || contents["de"] != testSRT || !strings.Contains(contents["tr"], "Turkish") {
		t.Errorf("subtitles = %v", contents)
	}
	if containsLine(jobLogLines(t, app, job.ID), "detected") {
		t.Error("the language was detected although it was given")
	}
}

func TestGenerateReplacesAnExistingSubtitle(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	if rec := app.upload(strconv.Itoa(videoID), "en", "srt", "a.srt", "1\n00:00:01,000 --> 00:00:02,000\nmanual\n"); rec.Code != 200 {
		t.Fatalf("upload: %d", rec.Code)
	}

	generate(app, videoID, map[string]any{"languages": []string{}})
	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if contents := subtitleContents(t, app, videoID); len(contents) != 1 || contents["en"] != testSRT {
		t.Fatalf("subtitles = %v", contents)
	}
}

func TestGenerateKeepsGoingWhenOneTranslationFails(t *testing.T) {
	tools := &fakeTools{}
	tools.translate = func(ctx context.Context, cues []subtitle.Cue, language string) ([]subtitle.Cue, error) {
		if language == "German" {
			return nil, errors.New("model refused")
		}
		return (&fakeTools{}).Translate(ctx, cues, language, func(string, ...any) {})
	}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)

	job := decodeJob(t, generate(app, videoID, map[string]any{"languages": []string{"de", "tr", "fr"}}))
	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	finished, _ := app.repo.GetJob(context.Background(), job.ID)
	if finished.Status != jobFailed || finished.Error != "translation failed for: de" {
		t.Fatalf("job = %+v", finished)
	}
	contents := subtitleContents(t, app, videoID)
	if _, found := contents["de"]; found || contents["en"] == "" || contents["tr"] == "" || contents["fr"] == "" {
		t.Errorf("subtitles = %v", contents)
	}
	if lines := jobLogLines(t, app, job.ID); !containsLine(lines, "error Translating to de failed: model refused") {
		t.Errorf("log:\n%s", strings.Join(lines, "\n"))
	}
}

func TestGenerateFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		tools *fakeTools
		want  string
	}{
		"download": {
			&fakeTools{download: func(context.Context, string, string, logFunc) (string, error) {
				return "", errors.New("yt-dlp failed: Sign in to confirm you're not a bot")
			}},
			"Sign in to confirm",
		},
		"transcription": {
			&fakeTools{transcribe: func(context.Context, string, string) (subtitle.Transcript, error) {
				return subtitle.Transcript{}, errors.New("empty transcription")
			}},
			"transcription failed: empty transcription",
		},
		"language not detected": {
			&fakeTools{transcribe: func(context.Context, string, string) (subtitle.Transcript, error) {
				return subtitle.Transcript{SRT: testSRT, Language: "English (US)"}, nil
			}},
			"couldn't tell the spoken language",
		},
	} {
		t.Run(name, func(t *testing.T) {
			app, runner := newJobApp(t, tc.tools)
			videoID := addTestVideo(t, app)
			job := decodeJob(t, generate(app, videoID, map[string]any{"languages": []string{"tr"}}))
			if err := runner.drain(context.Background()); err != nil {
				t.Fatal(err)
			}

			finished, _ := app.repo.GetJob(context.Background(), job.ID)
			if finished.Status != jobFailed || !strings.Contains(finished.Error, tc.want) {
				t.Fatalf("job = %+v", finished)
			}
			if contents := subtitleContents(t, app, videoID); len(contents) != 0 {
				t.Errorf("subtitles saved: %v", contents)
			}
			if lines := jobLogLines(t, app, job.ID); !containsLine(lines, "error Failed: ") {
				t.Errorf("log:\n%s", strings.Join(lines, "\n"))
			}
			// a failed job does not block the next one
			if rec := generate(app, videoID, map[string]any{"languages": []string{}}); rec.Code != 202 {
				t.Errorf("generate after a failure: %d", rec.Code)
			}
		})
	}
}

func TestGenerateRequestValidation(t *testing.T) {
	app, _ := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)

	if rec := app.postJSON(fmt.Sprintf("/api/admin/videos/%d/generate", videoID), map[string]any{}, false); rec.Code != 401 {
		t.Fatalf("without login: %d", rec.Code)
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
		"bad id":          {"/api/admin/videos/abc/generate", map[string]any{}, 400, "Invalid ID"},
		"unknown video":   {"/api/admin/videos/999/generate", map[string]any{}, 404, "Video not found"},
		"bad language":    {fmt.Sprintf("/api/admin/videos/%d/generate", videoID), map[string]any{"languages": []string{"turkish!"}}, 400, "Invalid language code"},
		"bad source":      {fmt.Sprintf("/api/admin/videos/%d/generate", videoID), map[string]any{"source_language": "x"}, 400, "Invalid source language"},
		"too many":        {fmt.Sprintf("/api/admin/videos/%d/generate", videoID), map[string]any{"languages": tooMany}, 400, "Too many languages"},
		"wrong json type": {fmt.Sprintf("/api/admin/videos/%d/generate", videoID), map[string]any{"languages": "tr"}, 400, "Invalid request"},
	} {
		rec := app.postJSON(tc.path, tc.payload, true)
		if rec.Code != tc.status || errorMessage(t, rec) != tc.message {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	if jobs, _ := app.repo.ListJobs(context.Background(), 10); len(jobs) != 0 {
		t.Errorf("rejected requests queued %d jobs", len(jobs))
	}
}

func TestGenerateRefusesDuplicateAndUnconfiguredJobs(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)

	if rec := generate(app, videoID, map[string]any{}); rec.Code != 202 {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := generate(app, videoID, map[string]any{}); rec.Code != 409 || !strings.Contains(errorMessage(t, rec), "already") {
		t.Fatalf("second while queued: %d %q", rec.Code, rec.Body.String())
	}
	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := generate(app, videoID, map[string]any{}); rec.Code != 202 {
		t.Fatalf("after it finished: %d", rec.Code)
	}

	unconfigured, _ := newJobApp(t, &fakeTools{ready: errors.New("OPENROUTER_API_KEY is not set")})
	id := addTestVideo(t, unconfigured)
	if rec := generate(unconfigured, id, map[string]any{}); rec.Code != 503 || errorMessage(t, rec) != "OPENROUTER_API_KEY is not set" {
		t.Fatalf("unconfigured: %d %q", rec.Code, rec.Body.String())
	}
}

func TestJobsAndLogsEndpoints(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	first := decodeJob(t, generate(app, videoID, map[string]any{"languages": []string{"tr"}}))
	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := decodeJob(t, generate(app, videoID, map[string]any{}))

	if rec := app.do(http.MethodGet, "/api/admin/jobs", nil, nil, false); rec.Code != 401 {
		t.Fatalf("jobs without login: %d", rec.Code)
	}
	var jobs []Job
	rec := app.do(http.MethodGet, "/api/admin/jobs", nil, nil, true)
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %v %q", err, rec.Body.String())
	}
	if jobs[0].ID != second.ID || jobs[0].Status != jobQueued || jobs[1].ID != first.ID || jobs[1].Status != jobDone || jobs[1].VideoTitle != "Me at the zoo" {
		t.Errorf("jobs = %+v", jobs)
	}

	logsPath := fmt.Sprintf("/api/admin/jobs/%d/logs", first.ID)
	if rec := app.do(http.MethodGet, logsPath, nil, nil, false); rec.Code != 401 {
		t.Fatalf("logs without login: %d", rec.Code)
	}
	var all struct {
		Job  Job
		Logs []JobLog
	}
	rec = app.do(http.MethodGet, logsPath, nil, nil, true)
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil || all.Job.Status != jobDone || len(all.Logs) < 5 {
		t.Fatalf("logs: %v %q", err, rec.Body.String())
	}
	if all.Logs[0].Message != "Started: Me at the zoo" || all.Logs[0].At == 0 || all.Logs[0].Level != logInfo {
		t.Errorf("first line = %+v", all.Logs[0])
	}

	// polling with the id of the last line seen returns only what is new
	var rest struct{ Logs []JobLog }
	rec = app.do(http.MethodGet, fmt.Sprintf("%s?after=%d", logsPath, all.Logs[2].ID), nil, nil, true)
	if err := json.Unmarshal(rec.Body.Bytes(), &rest); err != nil || len(rest.Logs) != len(all.Logs)-3 || rest.Logs[0].ID != all.Logs[3].ID {
		t.Fatalf("after: %v %q", err, rec.Body.String())
	}
	rec = app.do(http.MethodGet, fmt.Sprintf("%s?after=%d", logsPath, all.Logs[len(all.Logs)-1].ID), nil, nil, true)
	if !strings.Contains(rec.Body.String(), `"logs":[]`) {
		t.Errorf("nothing new: %q", rec.Body.String())
	}

	for path, status := range map[string]int{
		"/api/admin/jobs/999/logs": 404,
		"/api/admin/jobs/abc/logs": 400,
		logsPath + "?after=x":      400,
		logsPath + "?after=-1":     400,
	} {
		if rec := app.do(http.MethodGet, path, nil, nil, true); rec.Code != status {
			t.Errorf("%s: %d, want %d", path, rec.Code, status)
		}
	}
}

func TestInterruptedJobsAreFailedOnStart(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	ctx := context.Background()
	running, _ := app.repo.CreateJob(ctx, videoID, []string{}, "")
	if claimed, err := app.repo.ClaimNextJob(ctx); err != nil || claimed.ID != running {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	queued, _ := app.repo.CreateJob(ctx, videoID, []string{}, "")

	runCtx, stop := context.WithCancel(ctx)
	runner.Start(runCtx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, _ := app.repo.GetJob(ctx, queued)
		if job.Status == jobDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued job is %q", job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	runner.Wait()

	interrupted, _ := app.repo.GetJob(ctx, running)
	if interrupted.Status != jobFailed || interrupted.Error != "Interrupted by a restart" {
		t.Errorf("interrupted job = %+v", interrupted)
	}
}

func TestRunnerPicksUpJobsAddedWhileRunning(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	ctx, stop := context.WithCancel(context.Background())
	runner.Start(ctx)

	job := decodeJob(t, generate(app, videoID, map[string]any{"languages": []string{"tr"}}))
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, _ := app.repo.GetJob(context.Background(), job.ID)
		if current.Status == jobDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job is %q", current.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	runner.Wait()
	if contents := subtitleContents(t, app, videoID); len(contents) != 2 {
		t.Errorf("subtitles = %v", contents)
	}
}

func TestStoppingTheServerLeavesARunningJobForRecovery(t *testing.T) {
	started := make(chan struct{})
	tools := &fakeTools{download: func(ctx context.Context, _, _ string, _ logFunc) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	app, runner := newJobApp(t, tools)
	videoID := addTestVideo(t, app)
	job := decodeJob(t, generate(app, videoID, map[string]any{}))

	ctx, stop := context.WithCancel(context.Background())
	runner.Start(ctx)
	<-started
	stop()
	runner.Wait()

	current, _ := app.repo.GetJob(context.Background(), job.ID)
	if current.Status != jobRunning {
		t.Errorf("status = %q, want it left running so the next start fails it as interrupted", current.Status)
	}
}

func TestDeletingAVideoDeletesItsJobs(t *testing.T) {
	app, runner := newJobApp(t, &fakeTools{})
	videoID := addTestVideo(t, app)
	job := decodeJob(t, generate(app, videoID, map[string]any{}))
	if err := runner.drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	if rec := app.do(http.MethodDelete, fmt.Sprintf("/api/admin/videos/%d", videoID), nil, nil, true); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if jobs, _ := app.repo.ListJobs(context.Background(), 10); len(jobs) != 0 {
		t.Errorf("%d jobs left", len(jobs))
	}
	if logs, _ := app.repo.ListJobLogs(context.Background(), job.ID, 0, 10); len(logs) != 0 {
		t.Errorf("%d log lines left", len(logs))
	}
}

func TestUniqueLanguages(t *testing.T) {
	got := uniqueLanguages([]string{"tr", "en", "tr", "de", "en"}, "en")
	if strings.Join(got, ",") != "tr,de" {
		t.Errorf("got %v", got)
	}
}

func TestLanguageNames(t *testing.T) {
	if languageName("tr") != "Turkish" || languageName("pt-br") != "Brazilian Portuguese" {
		t.Error("known names")
	}
	if got := languageName("xx"); !strings.Contains(got, `"xx"`) {
		t.Errorf("unknown code described as %q", got)
	}
	for code, valid := range map[string]bool{"en": true, "pt-br": true, "eng": true, "e": false, "EN": false, "en_US": false, "english": false, "": false} {
		if languageCodePattern.MatchString(code) != valid {
			t.Errorf("%q valid = %v, want %v", code, !valid, valid)
		}
	}
}
