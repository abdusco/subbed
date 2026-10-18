package youtube

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
)

const (
	defaultOEmbedURL = "https://www.youtube.com/oembed"
	oembedTimeout    = 5 * time.Second
	maxOEmbedBytes   = 1 << 20
)

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ValidVideoID reports whether id has the shape of a YouTube video id.
func ValidVideoID(id string) bool {
	return videoIDPattern.MatchString(id)
}

var (
	ErrUnreachable = errors.New("Couldn't reach YouTube")
	ErrNotFound    = errors.New("Video not found or not public")
	ErrUnavailable = errors.New("YouTube is unavailable")
	ErrUnexpected  = errors.New("Unexpected answer from YouTube")
)

// OEmbedClient fetches video metadata from YouTube's oEmbed endpoint.
type OEmbedClient struct {
	client   *http.Client
	endpoint string
}

// NewOEmbedClient uses the default HTTP client and endpoint when they are omitted.
func NewOEmbedClient(client *http.Client, endpoint string) *OEmbedClient {
	if client == nil {
		client = &http.Client{Timeout: oembedTimeout}
	}
	return &OEmbedClient{client: client, endpoint: cmp.Or(endpoint, defaultOEmbedURL)}
}

// OEmbedResult contains the video's title and author.
type OEmbedResult struct {
	Title  string `json:"title"`
	Author string `json:"author"`
}

// Fetch looks up metadata for a YouTube video ID using the caller's context.
func (c *OEmbedClient) Fetch(ctx context.Context, videoID string) (OEmbedResult, error) {
	query := url.Values{
		"format": {"json"},
		"url":    {"https://www.youtube.com/watch?v=" + videoID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return OEmbedResult{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return OEmbedResult{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		// YouTube answers 400 for unknown ids, and 401/403 for private or embedding-disabled videos.
		return OEmbedResult{}, ErrNotFound
	default:
		return OEmbedResult{}, ErrUnavailable
	}

	var upstream struct {
		Title      string `json:"title"`
		AuthorName string `json:"author_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOEmbedBytes)).Decode(&upstream); err != nil {
		return OEmbedResult{}, fmt.Errorf("%w: %w", ErrUnexpected, err)
	}
	title := strings.TrimSpace(upstream.Title)
	if title == "" {
		return OEmbedResult{}, ErrUnexpected
	}

	return OEmbedResult{Title: title, Author: strings.TrimSpace(upstream.AuthorName)}, nil
}
