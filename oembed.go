package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

var (
	errOEmbedUnreachable = errors.New("Couldn't reach YouTube")
	errOEmbedNotFound    = errors.New("Video not found or not public")
	errOEmbedUnavailable = errors.New("YouTube is unavailable")
	errOEmbedUnexpected  = errors.New("Unexpected answer from YouTube")
)

// YoutubeOEmbed fetches video metadata from YouTube's oEmbed endpoint.
type YoutubeOEmbed struct {
	client   *http.Client
	endpoint string
}

// NewYoutubeOEmbed uses the default HTTP client and endpoint when they are omitted.
func NewYoutubeOEmbed(client *http.Client, endpoint string) *YoutubeOEmbed {
	if client == nil {
		client = &http.Client{Timeout: oembedTimeout}
	}
	return &YoutubeOEmbed{client: client, endpoint: cmp.Or(endpoint, defaultOEmbedURL)}
}

// YoutubeOEmbedResult contains the video's title and author.
type YoutubeOEmbedResult struct {
	Title  string `json:"title"`
	Author string `json:"author"`
}

// handleOEmbed looks up a video's title through YouTube's oEmbed endpoint, so the admin UI can fill it in.
// It is a small proxy: the browser talks to this server, and only a video id is ever sent upstream.
func handleOEmbed(client *YoutubeOEmbed) echo.HandlerFunc {
	return func(c echo.Context) error {
		videoID, ok := youtubeVideoIDFromURL(c.QueryParam("url"))
		if !ok || !videoIDPattern.MatchString(videoID) {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid YouTube URL")
		}

		result, err := client.Fetch(c.Request().Context(), videoID)
		if err != nil {
			switch {
			case errors.Is(err, errOEmbedNotFound):
				return echo.NewHTTPError(http.StatusNotFound, errOEmbedNotFound.Error()).SetInternal(err)
			case errors.Is(err, errOEmbedUnreachable):
				return echo.NewHTTPError(http.StatusBadGateway, errOEmbedUnreachable.Error()).SetInternal(err)
			case errors.Is(err, errOEmbedUnavailable):
				return echo.NewHTTPError(http.StatusBadGateway, errOEmbedUnavailable.Error()).SetInternal(err)
			case errors.Is(err, errOEmbedUnexpected):
				return echo.NewHTTPError(http.StatusBadGateway, errOEmbedUnexpected.Error()).SetInternal(err)
			default:
				return err
			}
		}

		return c.JSON(http.StatusOK, result)
	}
}

// Fetch looks up metadata for a YouTube video ID using the caller's context.
func (c *YoutubeOEmbed) Fetch(ctx context.Context, videoID string) (YoutubeOEmbedResult, error) {
	query := url.Values{
		"format": {"json"},
		"url":    {"https://www.youtube.com/watch?v=" + videoID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return YoutubeOEmbedResult{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return YoutubeOEmbedResult{}, fmt.Errorf("%w: %w", errOEmbedUnreachable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		// YouTube answers 400 for unknown ids, and 401/403 for private or embedding-disabled videos.
		return YoutubeOEmbedResult{}, errOEmbedNotFound
	default:
		return YoutubeOEmbedResult{}, errOEmbedUnavailable
	}

	var upstream struct {
		Title      string `json:"title"`
		AuthorName string `json:"author_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOEmbedBytes)).Decode(&upstream); err != nil {
		return YoutubeOEmbedResult{}, fmt.Errorf("%w: %w", errOEmbedUnexpected, err)
	}
	title := strings.TrimSpace(upstream.Title)
	if title == "" {
		return YoutubeOEmbedResult{}, errOEmbedUnexpected
	}

	return YoutubeOEmbedResult{Title: title, Author: strings.TrimSpace(upstream.AuthorName)}, nil
}
