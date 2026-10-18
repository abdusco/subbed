package main

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

const (
	maxSubtitleBytes = 2 << 20
	maxLanguageLen   = 16
)

// youtubeURLFromPath extracts the YouTube URL from a player path such as /https://youtube.com/watch.
// Browsers and proxies often collapse "https://" to "https:/", so that is repaired.
func youtubeURLFromPath(path string) (string, bool) {
	_, rest, found := strings.Cut(path, "http")
	if !found {
		return "", false
	}
	urlStr := "http" + rest

	if strings.HasPrefix(urlStr, "https:/") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = strings.Replace(urlStr, "https:/", "https://", 1)
	} else if strings.HasPrefix(urlStr, "http:/") && !strings.HasPrefix(urlStr, "http://") {
		urlStr = strings.Replace(urlStr, "http:/", "http://", 1)
	}

	return urlStr, true
}

// youtubeVideoIDFromURL returns the video id from a youtube.com watch URL or a youtu.be short URL.
func youtubeVideoIDFromURL(urlStr string) (string, bool) {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return "", false
	}
	host := strings.TrimPrefix(parsedURL.Hostname(), "www.")
	var videoID string
	switch host {
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		videoID = parsedURL.Query().Get("v")
	case "youtu.be":
		videoID = strings.TrimPrefix(parsedURL.Path, "/")
	}
	return videoID, videoID != ""
}

func handleVideoRequest(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()

		videoID, ok := youtubeVideoIDFromURL(c.QueryParam("url"))
		if !ok {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid YouTube URL")
		}

		video, err := repo.GetVideoByURL(ctx, videoID)
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "Video not found")
		}
		if err != nil {
			return err
		}

		subtitles, err := repo.GetSubtitlesByVideoID(ctx, video.ID)
		if err != nil {
			return err
		}

		return c.JSON(http.StatusOK, VideoResponse{
			Video: Video{
				ID:          video.ID,
				OriginalURL: videoID,
				Title:       video.Title,
			},
			Subtitles: subtitles,
		})
	}
}

func listVideos(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		videos, err := repo.ListAllVideos(c.Request().Context())
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, videos)
	}
}

func addVideo(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()

		var req struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		req.URL = strings.TrimSpace(req.URL)
		req.Title = strings.TrimSpace(req.Title)

		videoID, ok := youtubeVideoIDFromURL(req.URL)
		if !ok {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid YouTube URL")
		}
		if req.Title == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Title is required")
		}

		// the player finds videos by id, so the same video in another URL shape is a duplicate
		if _, err := repo.GetVideoByURL(ctx, videoID); err == nil {
			return echo.NewHTTPError(http.StatusConflict, "This video is already in the library")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		id, err := repo.CreateVideo(ctx, req.URL, req.Title)
		if err != nil {
			return err
		}

		return c.JSON(http.StatusOK, map[string]any{"id": id})
	}
}

func deleteVideo(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}

		if err := repo.DeleteVideo(c.Request().Context(), id); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"success": true})
	}
}

func uploadSubtitle(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		videoID, err := strconv.Atoi(c.FormValue("video_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid video ID")
		}

		language := strings.ToLower(strings.TrimSpace(c.FormValue("language")))
		if language == "" || len(language) > maxLanguageLen {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid language code")
		}

		fileType := strings.ToLower(strings.TrimSpace(c.FormValue("type")))
		if fileType == "" {
			fileType = "srt"
		}
		if fileType != "srt" && fileType != "vtt" {
			return echo.NewHTTPError(http.StatusBadRequest, "Type must be srt or vtt")
		}

		file, err := c.FormFile("file")
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "No file uploaded")
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		defer source.Close()

		data, err := io.ReadAll(io.LimitReader(source, maxSubtitleBytes+1))
		if err != nil {
			return err
		}
		if len(data) > maxSubtitleBytes {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "Subtitle file is too large")
		}

		content := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
		if fileType == "vtt" {
			content = vttToSRT(content)
		}

		// subtitles are always stored as SRT
		err = repo.CreateSubtitle(c.Request().Context(), videoID, language, "srt", content)
		if err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return echo.NewHTTPError(http.StatusNotFound, "Video not found")
		}
		if err != nil {
			return err
		}

		return c.JSON(http.StatusOK, map[string]any{"success": true})
	}
}

func deleteSubtitle(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}

		if err := repo.DeleteSubtitle(c.Request().Context(), id); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"success": true})
	}
}

// vttToSRT converts WebVTT to SRT: it drops the header, numbers the cues and uses commas in timestamps.
func vttToSRT(vtt string) string {
	lines := strings.Split(vtt, "\n")
	var srtLines []string
	counter := 1
	skipHeader := true

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)

		// Skip VTT header
		if skipHeader {
			if strings.HasPrefix(line, "WEBVTT") || line == "" {
				continue
			}
			skipHeader = false
		}

		if strings.Contains(line, "-->") {
			srtLines = append(srtLines, strconv.Itoa(counter))
			counter++

			// SRT uses a comma before the milliseconds
			line = strings.ReplaceAll(line, ".", ",")
		}
		srtLines = append(srtLines, line)
	}

	return strings.Join(srtLines, "\n")
}
