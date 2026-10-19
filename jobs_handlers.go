package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"subbed/internal/logger"
)

const (
	maxTargetLanguages = 20
	jobListSize        = 50
	jobLogsPageSize    = 500
)

// transcribeVideo queues a job that turns the video's audio into a subtitle in the spoken language.
func transcribeVideo(repo *Repository, runner *JobRunner) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req struct {
			SourceLanguage string `json:"source_language"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		source := strings.ToLower(strings.TrimSpace(req.SourceLanguage))
		if source != "" && !languageCodePattern.MatchString(source) {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid source language")
		}
		return queueJob(c, repo, runner, jobTranscribe, source, nil)
	}
}

// translateVideo queues a job that translates one of the video's subtitles into other languages.
func translateVideo(repo *Repository, runner *JobRunner) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req struct {
			SourceLanguage string   `json:"source_language"`
			Languages      []string `json:"languages"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		source := strings.ToLower(strings.TrimSpace(req.SourceLanguage))
		if !languageCodePattern.MatchString(source) {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid source language")
		}
		if len(req.Languages) > maxTargetLanguages {
			return echo.NewHTTPError(http.StatusBadRequest, "Too many languages")
		}
		targets := make([]string, len(req.Languages))
		for position, language := range req.Languages {
			targets[position] = strings.ToLower(strings.TrimSpace(language))
			if !languageCodePattern.MatchString(targets[position]) {
				return echo.NewHTTPError(http.StatusBadRequest, "Invalid language code")
			}
		}
		if targets = uniqueLanguages(targets, source); len(targets) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "Pick at least one language to translate into")
		}
		return queueJob(c, repo, runner, jobTranslate, source, targets)
	}
}

// queueJob checks that the video exists, that there is something to work with and that no other job is going for
// the video, then queues the job and answers with it. The request's :id is the video.
func queueJob(c echo.Context, repo *Repository, runner *JobRunner, kind, source string, targets []string) error {
	ctx := c.Request().Context()
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
	}
	if _, err := repo.GetVideoByID(ctx, videoID); errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "Video not found")
	} else if err != nil {
		return err
	}
	if kind == jobTranslate {
		if _, err := repo.GetSubtitle(ctx, videoID, source); errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "The video has no subtitle in that language")
		} else if err != nil {
			return err
		}
	}
	if err := runner.tools.Ready(); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
	}
	if active, err := repo.HasActiveJob(ctx, videoID); err != nil {
		return err
	} else if active {
		return echo.NewHTTPError(http.StatusConflict, "A job for this video is already queued or running")
	}

	id, err := repo.CreateJob(ctx, kind, videoID, targets, source)
	if err != nil {
		return err
	}
	logger.FromCtx(ctx).Info("Queued job", "job", id, "kind", kind, "video", videoID, "source", source, "targets", targets)
	runner.Notify()
	job, err := repo.GetJob(ctx, id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, job)
}

// stopJob makes sure a job is neither waiting nor working: a queued job is canceled, and a running one is stopped
// (this returns once it has ended). Any other job is left alone.
func stopJob(ctx context.Context, repo *Repository, runner *JobRunner, id int64) error {
	if _, err := repo.CancelQueuedJob(ctx, id); err != nil {
		return err
	}
	runner.Cancel(ctx, id)
	return nil
}

// cancelJob stops a job that is queued or running. A job that is over can't be canceled.
func cancelJob(repo *Repository, runner *JobRunner) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}
		job, err := repo.GetJob(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "Job not found")
		}
		if err != nil {
			return err
		}
		if !isJobActive(job.Status) {
			return echo.NewHTTPError(http.StatusConflict, "The job is already over")
		}
		if err := stopJob(ctx, repo, runner, id); err != nil {
			return err
		}
		logger.FromCtx(ctx).Info("Canceled job", "job", id)
		if job, err = repo.GetJob(ctx, id); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, job)
	}
}

// deleteJob removes a job and its log. A job that is queued or running is stopped first.
func deleteJob(repo *Repository, runner *JobRunner) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}
		if err := stopJob(ctx, repo, runner, id); err != nil {
			return err
		}
		deleted, err := repo.DeleteJob(ctx, id)
		if err != nil {
			return err
		}
		if !deleted {
			return echo.NewHTTPError(http.StatusNotFound, "Job not found")
		}
		logger.FromCtx(ctx).Info("Deleted job", "job", id)
		return c.JSON(http.StatusOK, map[string]any{"success": true})
	}
}

func isJobActive(status string) bool {
	return status == jobQueued || status == jobRunning
}

func listJobs(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		jobs, err := repo.ListJobs(c.Request().Context(), jobListSize)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, jobs)
	}
}

// getJobLogs returns a job and the log lines after the one with id `after`, so a page can poll for new lines.
func getJobLogs(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}
		var after int64
		if raw := c.QueryParam("after"); raw != "" {
			if after, err = strconv.ParseInt(raw, 10, 64); err != nil || after < 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "Invalid after")
			}
		}

		job, err := repo.GetJob(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "Job not found")
		}
		if err != nil {
			return err
		}
		logs, err := repo.ListJobLogs(ctx, id, after, jobLogsPageSize)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"job": job, "logs": logs})
	}
}
