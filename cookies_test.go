package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleCookies = "# Netscape HTTP Cookie File\n" +
	"# This is a generated file!  Do not edit.\n" +
	"\n" +
	".youtube.com\tTRUE\t/\tTRUE\t1893456000\tSID\tsecret-sid\n" +
	"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\t__Secure-1PSID\tsecret-psid\n" +
	"www.youtube.com\tFALSE\t/watch\tFALSE\t1893456000\tPREF\tf6=40000000\n" +
	".google.com\tTRUE\t/\tFALSE\t1893456000\tEMPTY\n"

func TestParseNetscapeCookies(t *testing.T) {
	got, err := parseNetscapeCookies(sampleCookies)
	require.NoError(t, err)
	assert.Equal(t, []Cookie{
		{Domain: ".youtube.com", Name: "SID", Value: "secret-sid", Path: "/", Secure: true, Expires: 1893456000},
		{Domain: ".youtube.com", Name: "__Secure-1PSID", Value: "secret-psid", Path: "/", Secure: true, HTTPOnly: true},
		{Domain: "www.youtube.com", Name: "PREF", Value: "f6=40000000", Path: "/watch", Expires: 1893456000},
		{Domain: ".google.com", Name: "EMPTY", Path: "/", Expires: 1893456000},
	}, got)
}

func TestParseNetscapeCookiesAcceptsWindowsLineEndings(t *testing.T) {
	got, err := parseNetscapeCookies(strings.ReplaceAll(sampleCookies, "\n", "\r\n"))
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, "secret-sid", got[0].Value)
}

func TestParseNetscapeCookiesRejectsBadInput(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"empty":             {"", "no cookies found"},
		"only comments":     {"# Netscape HTTP Cookie File\n", "no cookies found"},
		"too few fields":    {".a.com\tTRUE\t/\tTRUE\t1\n", "line 1: expected 7"},
		"spaces for tabs":   {".a.com TRUE / TRUE 1 name value\n", "line 1: expected 7"},
		"bad flag":          {".a.com\tTRUE\t/\tYES\t1\tn\tv\n", `"YES" should be TRUE or FALSE`},
		"bad expiry":        {"# header\n.a.com\tTRUE\t/\tTRUE\tsoon\tn\tv\n", "line 2: expiry"},
		"missing name":      {".a.com\tTRUE\t/\tTRUE\t1\t\tv\n", "domain and name are required"},
		"missing domain":    {"\tTRUE\t/\tTRUE\t1\tn\tv\n", "domain and name are required"},
		"too many fields":   {".a.com\tTRUE\t/\tTRUE\t1\tn\tv\textra\n", "found 8"},
		"json instead":      {`[{"name":"SID"}]`, "expected 7"},
		"html error page":   {"<html>\n<body>\n", "expected 7"},
		"first line is bad": {"nonsense\n.a.com\tTRUE\t/\tTRUE\t1\tn\tv\n", "line 1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseNetscapeCookies(tc.text)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRenderNetscapeCookiesRoundTrips(t *testing.T) {
	cookies, err := parseNetscapeCookies(sampleCookies)
	require.NoError(t, err)
	rendered := renderNetscapeCookies(cookies)
	assert.True(t, strings.HasPrefix(rendered, netscapeHeader+"\n"), "starts with the header")
	assert.Contains(t, rendered, "#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\t__Secure-1PSID\tsecret-psid\n")
	assert.Contains(t, rendered, "www.youtube.com\tFALSE\t/watch\tFALSE\t1893456000\tPREF\tf6=40000000\n")

	again, err := parseNetscapeCookies(rendered)
	require.NoError(t, err)
	assert.Equal(t, cookies, again)
}

func TestUpsertCookiesReplacesValuesAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	first, err := parseNetscapeCookies(sampleCookies)
	require.NoError(t, err)
	require.NoError(t, repo.UpsertCookies(ctx, first))

	update := ".youtube.com\tTRUE\t/\tTRUE\t1999999999\tSID\trotated-sid\n.youtube.com\tTRUE\t/\tFALSE\t0\tNEW\tfresh\n"
	second, err := parseNetscapeCookies(update)
	require.NoError(t, err)
	require.NoError(t, repo.UpsertCookies(ctx, second))

	stored, err := repo.ListCookies(ctx)
	require.NoError(t, err)
	require.Len(t, stored, 5)
	byName := map[string]Cookie{}
	for _, cookie := range stored {
		byName[cookie.Domain+" "+cookie.Name] = cookie
	}
	assert.Equal(t, "rotated-sid", byName[".youtube.com SID"].Value)
	assert.EqualValues(t, 1999999999, byName[".youtube.com SID"].Expires)
	assert.Equal(t, "secret-psid", byName[".youtube.com __Secure-1PSID"].Value, "an untouched cookie stays")
	assert.True(t, byName[".youtube.com __Secure-1PSID"].HTTPOnly)
	assert.Equal(t, "fresh", byName[".youtube.com NEW"].Value)
}

func TestSaveCookiesRejectsTooManyCookies(t *testing.T) {
	var text strings.Builder
	for number := range maxCookies + 1 {
		fmt.Fprintf(&text, ".a.com\tTRUE\t/\tFALSE\t0\tname%d\tv%d\n", number, number)
	}
	app := newTestApp(t)
	rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": text.String()}, true)
	assert.Equal(t, 400, rec.Code)
	assert.Equal(t, "Too many cookies", errorMessage(t, rec))
	stored, err := app.repo.ListCookies(context.Background())
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestWriteCookieFileIsPrivateAndRemovable(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)

	path, cleanup, err := writeCookieFile(ctx, repo)
	require.NoError(t, err)
	assert.Empty(t, path, "no file without cookies")
	cleanup()

	cookies, err := parseNetscapeCookies(sampleCookies)
	require.NoError(t, err)
	require.NoError(t, repo.UpsertCookies(ctx, cookies))
	path, cleanup, err = writeCookieFile(ctx, repo)
	require.NoError(t, err)
	require.NotEmpty(t, path)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(content), netscapeHeader))
	assert.Contains(t, string(content), "secret-sid")

	cleanup()
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "the file is removed")
}

func (a *testApp) putJSON(path string, payload any, authed bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	return a.do(http.MethodPut, path, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"}, authed)
}

func TestCookieEndpoints(t *testing.T) {
	app := newTestApp(t)

	assert.Equal(t, 401, app.putJSON("/api/admin/cookies", map[string]string{"cookies": sampleCookies}, false).Code, "put without login")
	assert.Equal(t, 401, app.do(http.MethodGet, "/api/admin/cookies", nil, nil, false).Code, "get without login")
	assert.JSONEq(t, `{"count":0,"domains":[]}`, app.do(http.MethodGet, "/api/admin/cookies", nil, nil, true).Body.String())

	rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": sampleCookies}, true)
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"count":4,"domains":["google.com","www.youtube.com","youtube.com"]}`, rec.Body.String())
	// values never leave the server
	for _, rec := range []*httptest.ResponseRecorder{rec, app.do(http.MethodGet, "/api/admin/cookies", nil, nil, true)} {
		assert.NotContains(t, rec.Body.String(), "secret")
		assert.NotContains(t, rec.Body.String(), "f6=")
	}

	// submitting again upserts
	update := ".youtube.com\tTRUE\t/\tTRUE\t1893456000\tSID\trotated\n"
	rec = app.putJSON("/api/admin/cookies", map[string]string{"cookies": update}, true)
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"count":4`)
	stored, err := app.repo.ListCookies(context.Background())
	require.NoError(t, err)
	for _, cookie := range stored {
		if cookie.Name == "SID" {
			assert.Equal(t, "rotated", cookie.Value)
		}
	}

	for name, payload := range map[string]any{
		"not cookies": map[string]string{"cookies": "hello"},
		"empty":       map[string]string{"cookies": ""},
		"wrong shape": map[string]int{"cookies": 5},
	} {
		rec := app.putJSON("/api/admin/cookies", payload, true)
		assert.Equal(t, 400, rec.Code, "%s: %s", name, rec.Body.String())
	}
	rec = app.putJSON("/api/admin/cookies", map[string]string{"cookies": "bad line"}, true)
	assert.True(t, strings.HasPrefix(errorMessage(t, rec), "Invalid cookies: line 1"))
	assert.Equal(t, 413, app.putJSON("/api/admin/cookies", map[string]string{"cookies": strings.Repeat("x", maxCookiesBytes+1)}, true).Code, "oversized")
	stored, err = app.repo.ListCookies(context.Background())
	require.NoError(t, err)
	assert.Len(t, stored, 4, "a rejected request changes nothing")

	assert.Equal(t, 401, app.do(http.MethodDelete, "/api/admin/cookies", nil, nil, false).Code, "delete without login")
	rec = app.do(http.MethodDelete, "/api/admin/cookies", nil, nil, true)
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"count":0,"domains":[]}`, rec.Body.String())
}

func TestSettingsEndpoints(t *testing.T) {
	app := newTestApp(t)

	assert.Equal(t, 401, app.do(http.MethodGet, "/api/admin/settings", nil, nil, false).Code, "get without login")
	assert.Equal(t, 401, app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": "x/y"}, false).Code, "put without login")

	read := func() settingsResponse {
		var settings settingsResponse
		rec := app.do(http.MethodGet, "/api/admin/settings", nil, nil, true)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
		return settings
	}
	assert.Equal(t, settingsResponse{TranscribeModelDefault: "openai/whisper-large-v3-turbo"}, read())
	model, err := app.repo.transcribeModel(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "openai/whisper-large-v3-turbo", model, "the default applies")

	rec := app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": "  openai/whisper-large-v3  "}, true)
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"transcribe_model":"openai/whisper-large-v3"`)
	model, err = app.repo.transcribeModel(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "openai/whisper-large-v3", model)
	assert.Equal(t, model, read().TranscribeModel)

	// a request without the field changes nothing
	assert.Equal(t, 200, app.putJSON("/api/admin/settings", map[string]string{}, true).Code)
	assert.Equal(t, "openai/whisper-large-v3", read().TranscribeModel)
	for name, bad := range map[string]string{"space inside": "two words", "newline": "a\nb", "too long": strings.Repeat("m", maxModelLen+1)} {
		assert.Equal(t, 400, app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": bad}, true).Code, name)
	}

	// an empty model restores the default
	assert.Equal(t, 200, app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": " "}, true).Code)
	assert.Empty(t, read().TranscribeModel)
}
