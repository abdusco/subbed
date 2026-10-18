package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"subbed/internal/subtitle"
	"subbed/internal/youtube"
)

const (
	maxLogMessageRunes = 2000
	retryClaimAfter    = 5 * time.Second
)

type logFunc = func(format string, args ...any)

// jobTools are the outside services a job uses.
type jobTools interface {
	// Ready reports why jobs cannot run, e.g. a missing API key
	Ready() error
	DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, logf logFunc) (string, error)
	Transcribe(ctx context.Context, model, path, language string, logf logFunc) (subtitle.Transcript, error)
	Translate(ctx context.Context, cues []subtitle.Cue, language string, logf logFunc) ([]subtitle.Cue, error)
}

// aiTools downloads with yt-dlp and calls OpenRouter models.
type aiTools struct {
	ytdlp          *youtube.YtDlp
	apiKey         string
	translateModel string
}

func (t aiTools) Ready() error {
	if t.apiKey == "" {
		return errors.New("OPENROUTER_API_KEY is not set")
	}
	return nil
}

func (t aiTools) DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, logf logFunc) (string, error) {
	return t.ytdlp.DownloadAudio(ctx, videoURL, directory, cookiesFile, logf)
}

func (t aiTools) Transcribe(ctx context.Context, model, path, language string, logf logFunc) (subtitle.Transcript, error) {
	transcriber := subtitle.NewTranscriber(subtitle.NewClient(t.apiKey, ""), model)
	transcriber.Language = language
	transcriber.Logf = logf
	return transcriber.Transcribe(ctx, path)
}

func (t aiTools) Translate(ctx context.Context, cues []subtitle.Cue, language string, logf logFunc) ([]subtitle.Cue, error) {
	translator := subtitle.NewTranslator(subtitle.NewClient(t.apiKey, ""), t.translateModel)
	translator.Logf = logf
	return translator.Translate(ctx, cues, language)
}

// JobRunner works through the queued jobs one at a time.
type JobRunner struct {
	repo   *Repository
	tools  jobTools
	logger *slog.Logger
	wake   chan struct{}
	wg     sync.WaitGroup
}

func NewJobRunner(repo *Repository, tools jobTools, logger *slog.Logger) *JobRunner {
	return &JobRunner{repo: repo, tools: tools, logger: logger, wake: make(chan struct{}, 1)}
}

// Start fails the jobs a previous run left unfinished, then processes jobs in the background until ctx is done.
func (j *JobRunner) Start(ctx context.Context) {
	j.wg.Go(func() {
		if err := j.repo.RecoverInterruptedJobs(ctx); err != nil {
			j.logger.Error("Couldn't recover jobs", "error", err)
		}
		for ctx.Err() == nil {
			if err := j.drain(ctx); err != nil {
				j.logger.Error("Job queue failed", "error", err)
				select {
				case <-time.After(retryClaimAfter):
				case <-ctx.Done():
				}
				continue
			}
			select {
			case <-j.wake:
			case <-ctx.Done():
			}
		}
	})
}

// Wait returns once the background work has stopped.
func (j *JobRunner) Wait() { j.wg.Wait() }

// Notify tells the runner that a job was queued.
func (j *JobRunner) Notify() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

// drain runs queued jobs until none is left or ctx is done.
func (j *JobRunner) drain(ctx context.Context) error {
	for ctx.Err() == nil {
		job, err := j.repo.ClaimNextJob(ctx)
		if err != nil || job == nil {
			return err
		}
		j.run(ctx, job)
	}
	return nil
}

func (j *JobRunner) run(ctx context.Context, job *Job) {
	log := &jobLog{repo: j.repo, logger: j.logger, jobID: job.ID}
	log.info(ctx, "Started: %s", job.VideoTitle)
	err := j.process(ctx, job, log)
	if ctx.Err() != nil {
		// the server is stopping; the job is failed as interrupted when it starts again
		return
	}
	status, message := jobDone, ""
	if err != nil {
		status, message = jobFailed, err.Error()
		log.error(ctx, "Failed: %s", message)
	} else {
		log.info(ctx, "Done")
	}
	if err := j.repo.FinishJob(ctx, job.ID, status, message); err != nil {
		j.logger.Error("Couldn't finish job", "job", job.ID, "error", err)
	}
}

// process downloads the audio, transcribes it and stores the subtitle, then translates it into each target
// language. A failed translation does not stop the others; the job then fails at the end.
func (j *JobRunner) process(ctx context.Context, job *Job, log *jobLog) error {
	video, err := j.repo.GetVideoByID(ctx, job.VideoID)
	if err != nil {
		return fmt.Errorf("couldn't load the video: %w", err)
	}
	videoID, ok := youtubeVideoIDFromURL(video.OriginalURL)
	if !ok {
		return fmt.Errorf("%q is not a YouTube URL", video.OriginalURL)
	}

	directory, err := os.MkdirTemp("", "subbed-job-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)

	cookiesFile, cleanup, err := writeCookieFile(ctx, j.repo)
	if err != nil {
		return fmt.Errorf("couldn't prepare the cookies: %w", err)
	}
	defer cleanup()
	if cookiesFile == "" {
		log.info(ctx, "No cookies stored; downloading as a signed-out visitor")
	}

	log.info(ctx, "Downloading the audio")
	audio, err := j.tools.DownloadAudio(ctx, "https://www.youtube.com/watch?v="+videoID, directory, cookiesFile, log.writer(ctx, "yt-dlp: "))
	if err != nil {
		return err
	}
	log.info(ctx, "Downloaded %s", audio[strings.LastIndex(audio, "/")+1:])

	model, err := j.repo.transcribeModel(ctx)
	if err != nil {
		return err
	}
	log.info(ctx, "Transcribing with %s", model)
	transcript, err := j.tools.Transcribe(ctx, model, audio, job.SourceLanguage, log.writer(ctx, ""))
	if err != nil {
		return fmt.Errorf("transcription failed: %w", err)
	}

	source := job.SourceLanguage
	if source == "" {
		source = strings.ToLower(strings.TrimSpace(transcript.Language))
		log.info(ctx, "The model detected the language %q", source)
	}
	if !languageCodePattern.MatchString(source) {
		return fmt.Errorf("couldn't tell the spoken language (the model said %q); set the source language and try again", source)
	}
	if err := j.repo.SaveSubtitle(ctx, video.ID, source, "srt", transcript.SRT); err != nil {
		return err
	}
	log.info(ctx, "Saved the %s subtitle (%d cues)", source, transcript.Cues)

	var failed []string
	for _, target := range uniqueLanguages(job.Languages, source) {
		if err := j.translate(ctx, video.ID, transcript.SRT, target, log); err != nil {
			log.error(ctx, "Translating to %s failed: %v", target, err)
			failed = append(failed, target)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("translation failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (j *JobRunner) translate(ctx context.Context, videoID int, srt, target string, log *jobLog) error {
	cues, err := subtitle.ParseSRT(srt)
	if err != nil {
		return err
	}
	log.info(ctx, "Translating %d cues to %s", len(cues), languageName(target))
	translated, err := j.tools.Translate(ctx, cues, languageName(target), log.writer(ctx, ""))
	if err != nil {
		return err
	}
	if err := j.repo.SaveSubtitle(ctx, videoID, target, "srt", subtitle.RenderSRT(translated)); err != nil {
		return err
	}
	log.info(ctx, "Saved the %s subtitle", target)
	return nil
}

// uniqueLanguages returns the targets without duplicates and without the source language.
func uniqueLanguages(targets []string, source string) []string {
	var unique []string
	for _, target := range targets {
		if target != source && !slices.Contains(unique, target) {
			unique = append(unique, target)
		}
	}
	return unique
}

// jobLog writes a job's log to the database. A line that can't be saved is only reported to the server log.
type jobLog struct {
	repo   *Repository
	logger *slog.Logger
	jobID  int64
}

func (l *jobLog) add(ctx context.Context, level, message string) {
	if utf8.RuneCountInString(message) > maxLogMessageRunes {
		message = string([]rune(message)[:maxLogMessageRunes]) + "…"
	}
	if err := l.repo.AddJobLog(ctx, l.jobID, level, message); err != nil {
		l.logger.Warn("Couldn't save a job log line", "job", l.jobID, "error", err)
	}
}

func (l *jobLog) info(ctx context.Context, format string, args ...any) {
	l.add(ctx, logInfo, fmt.Sprintf(format, args...))
}

func (l *jobLog) error(ctx context.Context, format string, args ...any) {
	l.add(ctx, logError, fmt.Sprintf(format, args...))
}

// writer returns a logf for tools that report progress; every line gets the prefix.
func (l *jobLog) writer(ctx context.Context, prefix string) logFunc {
	return func(format string, args ...any) { l.info(ctx, prefix+format, args...) }
}
