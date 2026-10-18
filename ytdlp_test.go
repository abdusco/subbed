package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type fakeYtDlp struct {
	version string
	err     error
}

func (f fakeYtDlp) Version(context.Context) (string, error) { return f.version, f.err }

func (f fakeYtDlp) Update(context.Context) (string, string, error) {
	return f.version, "Updated yt-dlp to nightly@" + f.version, f.err
}

func TestYtDlpEndpoints(t *testing.T) {
	app := newTestAppConfigured(t, "", fakeYtDlp{version: "2026.10.10"})

	if rec := app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, false); rec.Code != 401 {
		t.Fatalf("without login: %d", rec.Code)
	}
	if rec := app.do(http.MethodPost, "/api/admin/yt-dlp/update", nil, nil, false); rec.Code != 401 {
		t.Fatalf("update without login: %d", rec.Code)
	}
	rec := app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, true)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"installed":true,"version":"2026.10.10"}` {
		t.Fatalf("info: %d %q", rec.Code, rec.Body.String())
	}
	rec = app.do(http.MethodPost, "/api/admin/yt-dlp/update", nil, nil, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"version":"2026.10.10"`) || !strings.Contains(rec.Body.String(), "Updated yt-dlp") {
		t.Fatalf("update: %d %q", rec.Code, rec.Body.String())
	}
}

func TestYtDlpEndpointsReportAMissingBinary(t *testing.T) {
	app := newTestAppConfigured(t, "", fakeYtDlp{err: errors.New("not installed")})

	rec := app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, true)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"installed":false,"version":""}` {
		t.Fatalf("info: %d %q", rec.Code, rec.Body.String())
	}
	rec = app.do(http.MethodPost, "/api/admin/yt-dlp/update", nil, nil, true)
	if rec.Code != 502 || errorMessage(t, rec) != "Couldn't update yt-dlp" {
		t.Fatalf("update: %d %q", rec.Code, rec.Body.String())
	}
}

func TestYtDlpRoutesAreAbsentWithoutYtDlp(t *testing.T) {
	app := newTestApp(t)
	if rec := app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, true); rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
}
