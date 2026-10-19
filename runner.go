package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"subbed/internal/logger"
	"subbed/internal/subtitle"
	"subbed/internal/youtube"
)

const retryClaimAfter = 5 * time.Second

// progressFunc reports how much of some work is done (0 to 1) and what it is doing. An empty stage keeps the last one.
type progressFunc = func(fraction float64, stage string)

// jobTools are the outside services a job uses. They report their own progress as a fraction of what they do.
type jobTools interface {
	// Ready reports why jobs cannot run, e.g. a missing API key
	Ready() error
	DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, progress progressFunc) (string, error)
	Transcribe(ctx context.Context, model, path, language string, progress progressFunc) (subtitle.Transcript, error)
	Translate(ctx context.Context, model string, cues []subtitle.Cue, language string, progress progressFunc) ([]subtitle.Cue, error)
}

// aiTools downloads with yt-dlp and calls OpenRouter models.
type aiTools struct {
	ytdlp  *youtube.YtDlp
	apiKey string
}

func (t aiTools) Ready() error {
	if t.apiKey == "" {
		return errors.New("OPENROUTER_API_KEY is not set")
	}
	return nil
}

func (t aiTools) DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, progress progressFunc) (string, error) {
	return t.ytdlp.DownloadAudio(ctx, videoURL, directory, cookiesFile, func(fraction float64) { progress(fraction, "") })
}

func (t aiTools) Transcribe(ctx context.Context, model, path, language string, progress progressFunc) (subtitle.Transcript, error) {
	transcriber := subtitle.NewTranscriber(subtitle.NewClient(t.apiKey, ""), model)
	transcriber.Language = language
	transcriber.Progress = progress
	return transcriber.Transcribe(ctx, path)
}

func (t aiTools) Translate(ctx context.Context, model string, cues []subtitle.Cue, language string, progress progressFunc) ([]subtitle.Cue, error) {
	translator := subtitle.NewTranslator(subtitle.NewClient(t.apiKey, ""), model)
	translator.Progress = func(fraction float64) { progress(fraction, "") }
	return translator.Translate(ctx, cues, language)
}

// jobProgress stores one job's progress. The tools report in fractions of what they do; span places that on the
// part of the whole job it covers. Only a change of the whole percent or of the stage is written.
type jobProgress struct {
	repo   *Repository
	logger *slog.Logger
	jobID  int64

	mutex   sync.Mutex
	percent int
	stage   string
}

// set records that the job is fraction (0 to 1) done. An empty stage keeps the current one.
func (p *jobProgress) set(ctx context.Context, fraction float64, stage string) {
	percent := int(math.Round(min(max(fraction, 0), 1) * 100))
	p.mutex.Lock()
	defer p.mutex.Unlock()
	// the job never goes backwards, and a stage that reports nothing new leaves the old text
	percent = max(percent, p.percent)
	if stage == "" {
		stage = p.stage
	}
	if percent == p.percent && stage == p.stage {
		return
	}
	p.percent, p.stage = percent, stage
	// a canceled job can't write anymore, which is no cause for a warning
	if err := p.repo.SetJobProgress(ctx, p.jobID, percent, stage); err != nil && ctx.Err() == nil {
		p.logger.Warn("Couldn't save job progress", "job", p.jobID, "error", err)
	}
}

// span returns a progressFunc for work that covers the part of the job from `from` to `to` (both 0 to 1).
// While the work gives no stage of its own, the job shows defaultStage.
func (p *jobProgress) span(ctx context.Context, from, to float64, defaultStage string) progressFunc {
	return func(fraction float64, stage string) {
		if stage == "" {
			stage = defaultStage
		}
		p.set(ctx, from+(to-from)*min(max(fraction, 0), 1), stage)
	}
}

// JobRunner works through the queued jobs one at a time.
type JobRunner struct {
	repo   *Repository
	tools  jobTools
	logger *slog.Logger
	wake   chan struct{}
	wg     sync.WaitGroup

	// running is the job being worked on, if any
	mutex   sync.Mutex
	running *runningJob
}

// runningJob lets another goroutine stop the job that is being worked on and wait for it to end.
type runningJob struct {
	id     int64
	cancel context.CancelFunc
	done   chan struct{}
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

// Cancel stops the job with this id if it is the one being worked on, and waits until it has ended. It reports
// whether there was such a job. A job that is only queued is canceled with Repository.CancelQueuedJob.
func (j *JobRunner) Cancel(ctx context.Context, id int64) bool {
	j.mutex.Lock()
	running := j.running
	j.mutex.Unlock()
	if running == nil || running.id != id {
		return false
	}
	running.cancel()
	select {
	case <-running.done:
	case <-ctx.Done():
	}
	return true
}

// run works on a job. ctx ends when the server stops; the job's own context also ends when it is canceled, which
// stops yt-dlp and ffmpeg and abandons the API calls. How the job ended is recorded with ctx, which still works then.
func (j *JobRunner) run(ctx context.Context, job *Job) {
	ctx = withJobLog(ctx, j.repo, j.logger, job.ID)
	jobCtx, cancel := context.WithCancel(ctx)
	running := &runningJob{id: job.ID, cancel: cancel, done: make(chan struct{})}
	j.mutex.Lock()
	j.running = running
	j.mutex.Unlock()
	defer func() {
		cancel()
		j.mutex.Lock()
		j.running = nil
		j.mutex.Unlock()
		close(running.done)
	}()

	log := logger.FromCtx(ctx)
	log.Info("Started: " + job.VideoTitle)
	var err error
	switch job.Kind {
	case jobTranscribe:
		err = j.transcribe(jobCtx, job)
	case jobTranslate:
		err = j.translateExisting(jobCtx, job)
	default:
		err = fmt.Errorf("unknown job kind %q", job.Kind)
	}
	if ctx.Err() != nil {
		// the server is stopping; the job is failed as interrupted when it starts again
		return
	}
	status, message := jobDone, ""
	switch {
	case jobCtx.Err() != nil && err != nil:
		status = jobCanceled
		log.Info("Canceled")
	case err != nil:
		status, message = jobFailed, err.Error()
		log.Error("Failed: " + message)
	default:
		log.Info("Done")
	}
	if err := j.repo.FinishJob(ctx, job.ID, status, message); err != nil {
		j.logger.Error("Couldn't finish job", "job", job.ID, "error", err)
	}
}

// transcribe downloads the video's audio, transcribes it and stores the subtitle in the spoken language.
func (j *JobRunner) transcribe(ctx context.Context, job *Job) error {
	log := logger.FromCtx(ctx)
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
		log.Info("No cookies stored; downloading as a signed-out visitor")
	}

	// downloading takes the first third of the job, transcribing the rest
	const downloadShare = 0.35
	progress := &jobProgress{repo: j.repo, logger: j.logger, jobID: job.ID}
	progress.set(ctx, 0, "Downloading audio")
	log.Info("Downloading the audio")
	audio, err := j.tools.DownloadAudio(ctx, "https://www.youtube.com/watch?v="+videoID, directory, cookiesFile,
		progress.span(ctx, 0, downloadShare, "Downloading audio"))
	if err != nil {
		return err
	}
	log.Info("Downloaded " + audio[strings.LastIndex(audio, "/")+1:])

	model, err := j.repo.transcribeModel(ctx)
	if err != nil {
		return err
	}
	log.Info("Transcribing with " + model)
	progress.set(ctx, downloadShare, "Transcribing")
	transcript, err := j.tools.Transcribe(ctx, model, audio, job.SourceLanguage,
		progress.span(ctx, downloadShare, 1, "Transcribing"))
	if err != nil {
		return fmt.Errorf("transcription failed: %w", err)
	}

	source := job.SourceLanguage
	if source == "" {
		source = strings.ToLower(strings.TrimSpace(transcript.Language))
		log.Info(fmt.Sprintf("The model detected the language %q", source))
	}
	if !languageCodePattern.MatchString(source) {
		return fmt.Errorf("couldn't tell the spoken language (the model said %q); set the source language and try again", source)
	}
	if err := j.repo.SaveSubtitle(ctx, video.ID, source, "srt", transcript.SRT); err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Saved the %s subtitle (%d cues)", source, transcript.Cues))
	return nil
}

// translateExisting translates one of the video's subtitles into each target language. A failed translation does
// not stop the others; the job then fails at the end.
func (j *JobRunner) translateExisting(ctx context.Context, job *Job) error {
	log := logger.FromCtx(ctx)
	source, err := j.repo.GetSubtitle(ctx, job.VideoID, job.SourceLanguage)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("the video has no %s subtitle to translate", job.SourceLanguage)
	}
	if err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Translating the %s subtitle", source.Language))

	// each language takes an equal part of the job
	progress := &jobProgress{repo: j.repo, logger: j.logger, jobID: job.ID}
	targets := uniqueLanguages(job.Languages, source.Language)
	var failed []string
	for position, target := range targets {
		part := progress.span(ctx, float64(position)/float64(len(targets)), float64(position+1)/float64(len(targets)),
			fmt.Sprintf("Translating to %s", languageName(target)))
		part(0, "")
		if err := j.translate(ctx, job.VideoID, source.Content, target, part); err != nil {
			log.Error(fmt.Sprintf("Translating to %s failed: %v", target, err))
			failed = append(failed, target)
		}
		part(1, "")
	}
	if len(failed) > 0 {
		return fmt.Errorf("translation failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (j *JobRunner) translate(ctx context.Context, videoID int, srt, target string, progress progressFunc) error {
	log := logger.FromCtx(ctx)
	cues, err := subtitle.ParseSRT(srt)
	if err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Translating %d cues to %s", len(cues), languageName(target)))
	model, err := j.repo.translateModel(ctx)
	if err != nil {
		return err
	}
	log.Info("Translating with " + model)
	translated, err := j.tools.Translate(ctx, model, cues, languageName(target), progress)
	if err != nil {
		return err
	}
	if err := j.repo.SaveSubtitle(ctx, videoID, target, "srt", subtitle.RenderSRT(translated)); err != nil {
		return err
	}
	log.Info("Saved the " + target + " subtitle")
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
