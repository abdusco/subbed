package subtitle

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	requestTimeout = 5 * time.Minute
	maxAttempts    = 5
	maxBodyBytes   = 64 << 20
)

var retryStatuses = []int{429, 500, 502, 503, 504, 524, 529}

// Client calls the OpenRouter API.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// backoff is how long to wait after a failed attempt (1-based); tests replace it
	backoff func(attempt int) time.Duration
}

// NewClient uses the default endpoint when baseURL is empty.
func NewClient(apiKey, baseURL string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(cmp.Or(baseURL, defaultBaseURL), "/"),
		http:    &http.Client{Timeout: requestTimeout},
		backoff: func(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second },
	}
}

// Response is the final answer to a request. Statuses other than the retried ones are returned as they are,
// so callers can react to e.g. a 400.
type Response struct {
	Status int
	Body   []byte
}

// APIError reports that the API kept answering with a retryable status.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("OpenRouter answered %d: %.300s", e.Status, e.Body)
}

// Post sends payload as JSON to path (for example "/chat/completions"). Transport errors and the
// statuses 429 and 5xx are retried with a growing delay; once attempts run out the last error is returned.
func (c *Client) Post(ctx context.Context, path string, payload any) (Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	return retry(ctx, maxAttempts, c.backoff, func() (Response, bool, error) {
		response, err := c.send(ctx, path, body)
		if err != nil {
			return Response{}, true, err
		}
		if slices.Contains(retryStatuses, response.Status) {
			return Response{}, true, &APIError{Status: response.Status, Body: string(response.Body)}
		}
		return response, false, nil
	})
}

// retry runs call up to attempts times. call reports whether its error is worth retrying; between attempts it
// waits backoff(attempt). It stops early on success, a non-retryable error or a done context, and returns the
// last error once attempts run out.
func retry[T any](ctx context.Context, attempts int, backoff func(attempt int) time.Duration, call func() (T, bool, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		value, retryable, err := call()
		if err == nil {
			return value, nil
		}
		if !retryable {
			return zero, err
		}
		lastErr = err
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if attempt == attempts {
			break
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

func (c *Client) send(ctx context.Context, path string, body []byte) (Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	reply, err := c.http.Do(request)
	if err != nil {
		return Response{}, err
	}
	defer reply.Body.Close()
	data, err := io.ReadAll(io.LimitReader(reply.Body, maxBodyBytes))
	if err != nil {
		return Response{}, err
	}
	return Response{Status: reply.StatusCode, Body: data}, nil
}
