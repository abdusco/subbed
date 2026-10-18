package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
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

	assert.Equal(t, 401, app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, false).Code, "info without login")
	assert.Equal(t, 401, app.do(http.MethodPost, "/api/admin/yt-dlp/update", nil, nil, false).Code, "update without login")

	rec := app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, true)
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"installed":true,"version":"2026.10.10"}`, rec.Body.String())

	rec = app.do(http.MethodPost, "/api/admin/yt-dlp/update", nil, nil, true)
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"version":"2026.10.10"`)
	assert.Contains(t, rec.Body.String(), "Updated yt-dlp")
}

func TestYtDlpEndpointsReportAMissingBinary(t *testing.T) {
	app := newTestAppConfigured(t, "", fakeYtDlp{err: errors.New("not installed")})

	rec := app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, true)
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"installed":false,"version":""}`, rec.Body.String())

	rec = app.do(http.MethodPost, "/api/admin/yt-dlp/update", nil, nil, true)
	assert.Equal(t, 502, rec.Code)
	assert.Equal(t, "Couldn't update yt-dlp", errorMessage(t, rec))
}

func TestYtDlpRoutesAreAbsentWithoutYtDlp(t *testing.T) {
	app := newTestApp(t)
	assert.Equal(t, 404, app.do(http.MethodGet, "/api/admin/yt-dlp", nil, nil, true).Code)
}
