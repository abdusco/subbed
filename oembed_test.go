package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestYoutubeOEmbedFetch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   YoutubeOEmbedResult
		err    error
	}{
		{name: "metadata", status: 200, body: `{"title":"  Me at the zoo ","author_name":" jawed "}`, want: YoutubeOEmbedResult{Title: "Me at the zoo", Author: "jawed"}},
		{name: "unknown video", status: 400, err: errOEmbedNotFound},
		{name: "private video", status: 401, err: errOEmbedNotFound},
		{name: "embedding disabled", status: 403, err: errOEmbedNotFound},
		{name: "missing video", status: 404, err: errOEmbedNotFound},
		{name: "outage", status: 500, err: errOEmbedUnavailable},
		{name: "invalid json", status: 200, body: `not json`, err: errOEmbedUnexpected},
		{name: "empty title", status: 200, body: `{"title":" "}`, err: errOEmbedUnexpected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("url") != "https://www.youtube.com/watch?v=jNQXAC9IVRw" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			client := NewYoutubeOEmbed(upstream.Client(), upstream.URL)
			got, err := client.Fetch(context.Background(), "jNQXAC9IVRw")
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Errorf("result = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestYoutubeOEmbedFetchCanceled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("canceled request reached upstream")
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewYoutubeOEmbed(upstream.Client(), upstream.URL)
	_, err := client.Fetch(ctx, "jNQXAC9IVRw")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errOEmbedUnreachable) {
		t.Fatalf("error = %v, want cancellation and unreachable errors", err)
	}
}
