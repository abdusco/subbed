package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOEmbedClientFetch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   OEmbedResult
		err    error
	}{
		{name: "metadata", status: 200, body: `{"title":"  Me at the zoo ","author_name":" jawed "}`, want: OEmbedResult{Title: "Me at the zoo", Author: "jawed"}},
		{name: "unknown video", status: 400, err: ErrNotFound},
		{name: "private video", status: 401, err: ErrNotFound},
		{name: "embedding disabled", status: 403, err: ErrNotFound},
		{name: "missing video", status: 404, err: ErrNotFound},
		{name: "outage", status: 500, err: ErrUnavailable},
		{name: "invalid json", status: 200, body: `not json`, err: ErrUnexpected},
		{name: "empty title", status: 200, body: `{"title":" "}`, err: ErrUnexpected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "json", r.URL.Query().Get("format"))
				assert.Equal(t, "https://www.youtube.com/watch?v=jNQXAC9IVRw", r.URL.Query().Get("url"))
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			client := NewOEmbedClient(upstream.Client(), upstream.URL)
			got, err := client.Fetch(context.Background(), "jNQXAC9IVRw")
			assert.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOEmbedClientFetchCanceled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("canceled request reached upstream")
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewOEmbedClient(upstream.Client(), upstream.URL)
	_, err := client.Fetch(ctx, "jNQXAC9IVRw")
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, ErrUnreachable)
}

func TestValidVideoID(t *testing.T) {
	for id, want := range map[string]bool{"jNQXAC9IVRw": true, "a46Xp8FtWOg": true, "short": false, "has space!!": false, "": false} {
		assert.Equal(t, want, ValidVideoID(id), "%q", id)
	}
}
