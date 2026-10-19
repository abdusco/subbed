package subtitle

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"subbed/internal/logger"
)

func newTestClient(handler http.HandlerFunc) (*Client, *httptest.Server) {
	server := httptest.NewServer(handler)
	client := NewClient("test-key", server.URL+"/")
	client.backoff = func(int) time.Duration { return time.Millisecond }
	return client, server
}

func TestPostSendsAuthorizedJSON(t *testing.T) {
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		assert.Equal(t, "m", payload["model"])
		w.Write([]byte(`{"ok":true}`))
	})
	defer server.Close()

	response, err := client.Post(context.Background(), "/chat/completions", map[string]string{"model": "m"})
	require.NoError(t, err)
	assert.Equal(t, 200, response.Status)
	assert.Equal(t, `{"ok":true}`, string(response.Body))
}

func TestPostRetriesTransientStatuses(t *testing.T) {
	var calls atomic.Int32
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("done"))
	})
	defer server.Close()

	response, err := client.Post(context.Background(), "/x", map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "done", string(response.Body))
	assert.EqualValues(t, 3, calls.Load())
}

func TestPostReportsRetries(t *testing.T) {
	var calls atomic.Int32
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("done"))
	})
	defer server.Close()
	logs := &bytes.Buffer{}
	ctx := logger.ToCtx(context.Background(), slog.New(slog.NewTextHandler(logs, nil)))

	_, err := client.Post(ctx, "/x", map[string]string{})
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "level=WARN")
	assert.Contains(t, lines[0], "attempt 1/5")
	assert.Contains(t, lines[0], "429")
}

func TestPostReturnsClientErrorsWithoutRetrying(t *testing.T) {
	var calls atomic.Int32
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad model"))
	})
	defer server.Close()

	response, err := client.Post(context.Background(), "/x", map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, 400, response.Status)
	assert.Equal(t, "bad model", string(response.Body))
	assert.EqualValues(t, 1, calls.Load())
}

func TestPostGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("slow down"))
	})
	defer server.Close()

	_, err := client.Post(context.Background(), "/x", map[string]string{})
	var apiError *APIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, 429, apiError.Status)
	assert.EqualValues(t, maxAttempts, calls.Load())
}

func TestPostStopsWhenCanceled(t *testing.T) {
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer server.Close()
	client.backoff = func(int) time.Duration { return time.Hour }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Post(ctx, "/x", map[string]string{})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
