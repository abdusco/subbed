package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"subbed/internal/youtube"
)

const (
	maxSubtitleBytes = 2 << 20
	maxLanguageLen   = 16
	maxTitleLen      = 300
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

// handleOEmbed looks up a video's title through YouTube's oEmbed endpoint, so the admin UI can fill it in.
// It is a small proxy: the browser talks to this server, and only a video id is ever sent upstream.
func handleOEmbed(client *youtube.OEmbedClient) echo.HandlerFunc {
	return func(c echo.Context) error {
		videoID, ok := youtubeVideoIDFromURL(c.QueryParam("url"))
		if !ok || !youtube.ValidVideoID(videoID) {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid YouTube URL")
		}

		result, err := client.Fetch(c.Request().Context(), videoID)
		if err != nil {
			switch {
			case errors.Is(err, youtube.ErrNotFound):
				return echo.NewHTTPError(http.StatusNotFound, youtube.ErrNotFound.Error()).SetInternal(err)
			case errors.Is(err, youtube.ErrUnreachable):
				return echo.NewHTTPError(http.StatusBadGateway, youtube.ErrUnreachable.Error()).SetInternal(err)
			case errors.Is(err, youtube.ErrUnavailable):
				return echo.NewHTTPError(http.StatusBadGateway, youtube.ErrUnavailable.Error()).SetInternal(err)
			case errors.Is(err, youtube.ErrUnexpected):
				return echo.NewHTTPError(http.StatusBadGateway, youtube.ErrUnexpected.Error()).SetInternal(err)
			default:
				return err
			}
		}

		return c.JSON(http.StatusOK, result)
	}
}

// ytDlpTool is the part of yt-dlp the admin routes use.
type ytDlpTool interface {
	Version(ctx context.Context) (string, error)
	Update(ctx context.Context) (version, output string, err error)
}

type ytDlpStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
}

func ytDlpInfo(ytdlp ytDlpTool) echo.HandlerFunc {
	return func(c echo.Context) error {
		version, err := ytdlp.Version(c.Request().Context())
		return c.JSON(http.StatusOK, ytDlpStatus{Installed: err == nil, Version: version})
	}
}

func ytDlpUpdate(ytdlp ytDlpTool) echo.HandlerFunc {
	return func(c echo.Context) error {
		version, output, err := ytdlp.Update(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "Couldn't update yt-dlp").SetInternal(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"installed": true, "version": version, "output": output})
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

// updateVideo changes a video's title.
func updateVideo(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}

		var req struct {
			Title string `json:"title"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Title is required")
		}
		if utf8.RuneCountInString(title) > maxTitleLen {
			return echo.NewHTTPError(http.StatusBadRequest, "Title is too long")
		}

		found, err := repo.UpdateVideoTitle(c.Request().Context(), id, title)
		if err != nil {
			return err
		}
		if !found {
			return echo.NewHTTPError(http.StatusNotFound, "Video not found")
		}
		return c.JSON(http.StatusOK, map[string]any{"id": id, "title": title})
	}
}

// deleteVideo removes a video with its subtitles and jobs. A job that is still going for it is stopped first, so it
// doesn't go on working for a video that is gone. runner may be nil.
func deleteVideo(repo *Repository, runner *JobRunner) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID")
		}

		if runner != nil {
			active, err := repo.ActiveJobIDs(c.Request().Context(), id)
			if err != nil {
				return err
			}
			for _, jobID := range active {
				if err := stopJob(c.Request().Context(), repo, runner, jobID); err != nil {
					return err
				}
			}
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

		// subtitles are always stored as SRT; a new upload replaces the video's subtitle in that language
		err = repo.SaveSubtitle(c.Request().Context(), videoID, language, "srt", content)
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
