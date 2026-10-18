package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/doug-martin/goqu/v9"
)

const (
	jobQueued  = "queued"
	jobRunning = "running"
	jobDone    = "done"
	jobFailed  = "failed"
)

const (
	logInfo  = "info"
	logError = "error"
)

// Job is a request to generate subtitles for a video.
type Job struct {
	ID         int64  `json:"id" db:"id"`
	VideoID    int    `json:"video_id" db:"video_id"`
	VideoTitle string `json:"video_title" db:"video_title"`
	// Status is queued, running, done or failed
	Status string `json:"status" db:"status"`
	// Languages are the translation targets
	Languages     []string `json:"languages" db:"-"`
	LanguagesJSON string   `json:"-" db:"languages"`
	// SourceLanguage is what the admin said the video is spoken in; empty means detect it
	SourceLanguage string `json:"source_language" db:"source_language"`
	Error          string `json:"error" db:"error"`
	CreatedAt      int64  `json:"created_at" db:"created_at"`
	UpdatedAt      int64  `json:"updated_at" db:"updated_at"`
}

// JobLog is one line of a job's log.
type JobLog struct {
	ID int64 `json:"id" db:"id"`
	// At is a Unix time in milliseconds
	At      int64  `json:"at" db:"at"`
	Level   string `json:"level" db:"level"`
	Message string `json:"message" db:"message"`
}

func (j *Job) decode() error {
	if err := json.Unmarshal([]byte(j.LanguagesJSON), &j.Languages); err != nil {
		return fmt.Errorf("job %d has invalid languages: %w", j.ID, err)
	}
	if j.Languages == nil {
		j.Languages = []string{}
	}
	return nil
}

// jobQuery selects jobs together with their video's title.
func (r *Repository) jobQuery() *goqu.SelectDataset {
	return r.db.From("jobs").
		Join(goqu.T("videos"), goqu.On(goqu.I("jobs.video_id").Eq(goqu.I("videos.id")))).
		Select(
			goqu.I("jobs.id").As("id"),
			goqu.I("jobs.video_id").As("video_id"),
			goqu.I("videos.title").As("video_title"),
			goqu.I("jobs.status").As("status"),
			goqu.I("jobs.languages").As("languages"),
			goqu.I("jobs.source_language").As("source_language"),
			goqu.I("jobs.error").As("error"),
			goqu.I("jobs.created_at").As("created_at"),
			goqu.I("jobs.updated_at").As("updated_at"),
		)
}

// CreateJob queues a job.
func (r *Repository) CreateJob(ctx context.Context, videoID int, languages []string, sourceLanguage string) (int64, error) {
	encoded, err := json.Marshal(languages)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	result, err := r.db.Insert("jobs").
		Rows(goqu.Record{
			"video_id": videoID, "status": jobQueued, "languages": string(encoded), "source_language": sourceLanguage,
			"error": "", "created_at": now, "updated_at": now,
		}).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to insert job: %w", err)
	}
	return result.LastInsertId()
}

// GetJob returns a job, or sql.ErrNoRows.
func (r *Repository) GetJob(ctx context.Context, id int64) (*Job, error) {
	var job Job
	found, err := r.jobQuery().Where(goqu.I("jobs.id").Eq(id)).ScanStructContext(ctx, &job)
	if err != nil {
		return nil, fmt.Errorf("failed to query job: %w", err)
	}
	if !found {
		return nil, sql.ErrNoRows
	}
	return &job, job.decode()
}

// ListJobs returns the newest jobs first.
func (r *Repository) ListJobs(ctx context.Context, limit uint) ([]Job, error) {
	var jobs []Job
	err := r.jobQuery().Order(goqu.I("jobs.id").Desc()).Limit(limit).ScanStructsContext(ctx, &jobs)
	if err != nil {
		return nil, fmt.Errorf("failed to query jobs: %w", err)
	}
	if jobs == nil {
		jobs = []Job{}
	}
	for position := range jobs {
		if err := jobs[position].decode(); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// HasActiveJob reports whether the video has a job that is queued or running.
func (r *Repository) HasActiveJob(ctx context.Context, videoID int) (bool, error) {
	count, err := r.db.From("jobs").
		Where(goqu.C("video_id").Eq(videoID), goqu.C("status").In(jobQueued, jobRunning)).
		CountContext(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to count jobs: %w", err)
	}
	return count > 0, nil
}

// ClaimNextJob marks the oldest queued job as running and returns it, or nil if none is waiting.
// Only one runner claims jobs, so reading and then updating cannot race.
func (r *Repository) ClaimNextJob(ctx context.Context) (*Job, error) {
	var job Job
	found, err := r.jobQuery().
		Where(goqu.I("jobs.status").Eq(jobQueued)).
		Order(goqu.I("jobs.id").Asc()).
		Limit(1).
		ScanStructContext(ctx, &job)
	if err != nil {
		return nil, fmt.Errorf("failed to query next job: %w", err)
	}
	if !found {
		return nil, nil
	}
	if err := r.setJobStatus(ctx, job.ID, jobRunning, ""); err != nil {
		return nil, err
	}
	job.Status = jobRunning
	return &job, job.decode()
}

// FinishJob records how a job ended: done, or failed with an error message.
func (r *Repository) FinishJob(ctx context.Context, id int64, status, errorMessage string) error {
	return r.setJobStatus(ctx, id, status, errorMessage)
}

func (r *Repository) setJobStatus(ctx context.Context, id int64, status, errorMessage string) error {
	_, err := r.db.Update("jobs").
		Set(goqu.Record{"status": status, "error": errorMessage, "updated_at": time.Now().Unix()}).
		Where(goqu.C("id").Eq(id)).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}
	return nil
}

// RecoverInterruptedJobs fails the jobs that were running when the server stopped; queued jobs stay queued.
func (r *Repository) RecoverInterruptedJobs(ctx context.Context) error {
	_, err := r.db.Update("jobs").
		Set(goqu.Record{"status": jobFailed, "error": "Interrupted by a restart", "updated_at": time.Now().Unix()}).
		Where(goqu.C("status").Eq(jobRunning)).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to recover jobs: %w", err)
	}
	return nil
}

// AddJobLog appends a line to a job's log.
func (r *Repository) AddJobLog(ctx context.Context, jobID int64, level, message string) error {
	_, err := r.db.Insert("job_logs").
		Rows(goqu.Record{"job_id": jobID, "at": time.Now().UnixMilli(), "level": level, "message": message}).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to insert job log: %w", err)
	}
	return nil
}

// ListJobLogs returns up to limit log lines of a job that come after the line with id `after` (0 for all).
func (r *Repository) ListJobLogs(ctx context.Context, jobID, after int64, limit uint) ([]JobLog, error) {
	var logs []JobLog
	err := r.db.From("job_logs").
		Select("id", "at", "level", "message").
		Where(goqu.C("job_id").Eq(jobID), goqu.C("id").Gt(after)).
		Order(goqu.C("id").Asc()).
		Limit(limit).
		ScanStructsContext(ctx, &logs)
	if err != nil {
		return nil, fmt.Errorf("failed to query job logs: %w", err)
	}
	if logs == nil {
		logs = []JobLog{}
	}
	return logs, nil
}

// GetVideoByID returns a video, or sql.ErrNoRows.
func (r *Repository) GetVideoByID(ctx context.Context, id int) (*Video, error) {
	var video Video
	found, err := r.db.From("videos").
		Select("id", "original_url", "title").
		Where(goqu.C("id").Eq(id)).
		ScanStructContext(ctx, &video)
	if err != nil {
		return nil, fmt.Errorf("failed to query video: %w", err)
	}
	if !found {
		return nil, sql.ErrNoRows
	}
	return &video, nil
}
