package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	defaultOEmbedURL = "https://www.youtube.com/oembed"
	oembedTimeout    = 5 * time.Second
	maxOEmbedBytes   = 1 << 20
)

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// oembedResult is what the admin UI needs from YouTube's oEmbed answer.
type oembedResult struct {
	Title  string `json:"title"`
	Author string `json:"author"`
}

// handleOEmbed looks up a video's title through YouTube's oEmbed endpoint, so the admin UI can fill it in.
// It is a small proxy: the browser talks to this server, and only a video id is ever sent upstream.
func handleOEmbed(client *http.Client, endpoint string) echo.HandlerFunc {
	return func(c echo.Context) error {
		videoID, ok := youtubeVideoIDFromURL(c.QueryParam("url"))
		if !ok || !videoIDPattern.MatchString(videoID) {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid YouTube URL")
		}

		query := url.Values{
			"format": {"json"},
			"url":    {"https://www.youtube.com/watch?v=" + videoID},
		}
		req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "Couldn't reach YouTube").SetInternal(err)
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			// YouTube answers 400 for unknown ids, and 401/403 for private or embedding-disabled videos
			return echo.NewHTTPError(http.StatusNotFound, "Video not found or not public")
		default:
			return echo.NewHTTPError(http.StatusBadGateway, "YouTube is unavailable")
		}

		var upstream struct {
			Title      string `json:"title"`
			AuthorName string `json:"author_name"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxOEmbedBytes)).Decode(&upstream); err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "Unexpected answer from YouTube").SetInternal(err)
		}
		title := strings.TrimSpace(upstream.Title)
		if title == "" {
			return echo.NewHTTPError(http.StatusBadGateway, "Unexpected answer from YouTube")
		}

		return c.JSON(http.StatusOK, oembedResult{Title: title, Author: strings.TrimSpace(upstream.AuthorName)})
	}
}
