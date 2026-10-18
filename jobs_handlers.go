package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

const (
	maxTargetLanguages = 20
	jobListSize        = 50
	jobLogsPageSize    = 500
)

// generateSubtitles queues a job that transcribes the video and translates the result.
func generateSubtitles(repo *Repository, runner *JobRunner) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		videoID, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}

		var req struct {
			Languages      []string `json:"languages"`
			SourceLanguage string   `json:"source_language"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		source := strings.ToLower(strings.TrimSpace(req.SourceLanguage))
		if source != "" && !languageCodePattern.MatchString(source) {
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

		if _, err := repo.GetVideoByID(ctx, videoID); errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "Video not found")
		} else if err != nil {
			return err
		}
		if err := runner.tools.Ready(); err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
		}
		if active, err := repo.HasActiveJob(ctx, videoID); err != nil {
			return err
		} else if active {
			return echo.NewHTTPError(http.StatusConflict, "A job for this video is already queued or running")
		}

		id, err := repo.CreateJob(ctx, videoID, uniqueLanguages(targets, source), source)
		if err != nil {
			return err
		}
		runner.Notify()
		job, err := repo.GetJob(ctx, id)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusAccepted, job)
	}
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
