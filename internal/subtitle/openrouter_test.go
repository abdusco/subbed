package subtitle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
		json.NewDecoder(r.Body).Decode(&payload)
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" || payload["model"] != "m" {
			t.Errorf("unexpected request: %s %v %v", r.URL.Path, r.Header, payload)
		}
		w.Write([]byte(`{"ok":true}`))
	})
	defer server.Close()

	response, err := client.Post(context.Background(), "/chat/completions", map[string]string{"model": "m"})
	if err != nil || response.Status != 200 || string(response.Body) != `{"ok":true}` {
		t.Fatalf("response = %+v, %v", response, err)
	}
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
	if err != nil || string(response.Body) != "done" || calls.Load() != 3 {
		t.Fatalf("response = %+v, %v after %d calls", response, err, calls.Load())
	}
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
	if err != nil || response.Status != 400 || string(response.Body) != "bad model" || calls.Load() != 1 {
		t.Fatalf("response = %+v, %v after %d calls", response, err, calls.Load())
	}
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
	if !errors.As(err, &apiError) || apiError.Status != 429 || calls.Load() != maxAttempts {
		t.Fatalf("error = %v after %d calls", err, calls.Load())
	}
}

func TestPostStopsWhenCanceled(t *testing.T) {
	client, server := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer server.Close()
	client.backoff = func(int) time.Duration { return time.Hour }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.Post(ctx, "/x", map[string]string{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
}
