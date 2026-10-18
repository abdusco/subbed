package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"subbed/internal/youtube"
)

var (
	testCreds = Credentials{Username: "admin", Password: "secret"}
	testKey   = []byte("test-session-key")
)

type testApp struct {
	t       *testing.T
	handler http.Handler
	logs    *bytes.Buffer
	repo    *Repository
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppWith(t, "")
}

func newTestAppWith(t *testing.T, oembedURL string) *testApp {
	t.Helper()
	return newTestAppConfigured(t, oembedURL, nil)
}

func newTestAppConfigured(t *testing.T, oembedURL string, ytdlp ytDlpTool) *testApp {
	t.Helper()
	return newTestAppFor(t, func(cfg *serverConfig) {
		cfg.oembed = youtube.NewOEmbedClient(nil, oembedURL)
		cfg.ytdlp = ytdlp
	})
}

// newTestAppFor builds the app with test defaults; configure may change the config (the repository is already set).
func newTestAppFor(t *testing.T, configure func(cfg *serverConfig)) *testApp {
	t.Helper()
	repo, err := NewRepository(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })

	assets := fstest.MapFS{
		"index.html":          {Data: []byte("<title>player</title>")},
		"admin.html":          {Data: []byte("<title>admin</title>")},
		"login.html":          {Data: []byte("<title>login</title>")},
		"admin-sw.js":         {Data: []byte("// sw")},
		"app.js":              {Data: []byte("// app")},
		"admin/admin.css":     {Data: []byte("body{}")},
		"admin.manifest.json": {Data: []byte("{}")},
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	cfg := serverConfig{repo: repo, assets: assets, creds: testCreds, sessionKey: testKey, logger: logger}
	configure(&cfg)
	e := newServer(cfg)
	return &testApp{t: t, handler: e, logs: logs, repo: repo}
}

func (a *testApp) do(method, target string, body io.Reader, headers map[string]string, authed bool) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, target, body)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if authed {
		token, _ := newSessions(testCreds, testKey).issue()
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func (a *testApp) addVideo(rawURL, title string) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]string{"url": rawURL, "title": title})
	return a.do(http.MethodPost, "/api/admin/videos", bytes.NewReader(payload), map[string]string{"Content-Type": "application/json"}, true)
}

func (a *testApp) upload(videoID, language, fileType, filename, content string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	form.WriteField("video_id", videoID)
	form.WriteField("language", language)
	form.WriteField("type", fileType)
	part, _ := form.CreateFormFile("file", filename)
	part.Write([]byte(content))
	form.Close()
	return a.do(http.MethodPost, "/api/admin/subtitles", &body, map[string]string{"Content-Type": form.FormDataContentType()}, true)
}

// errorMessage decodes an API error body and checks it is the documented {"error": "..."} object.
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), "error Content-Type")
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.Len(t, body, 1, `want {"error": "..."}: %s`, rec.Body.String())
	require.NotEmpty(t, body["error"])
	return body["error"]
}

func TestPagesAndStaticFiles(t *testing.T) {
	app := newTestApp(t)

	cases := []struct {
		name   string
		target string
		authed bool
		status int
		body   string
	}{
		{"player", "/", false, 200, "player"},
		{"player for a youtube path", "/https://www.youtube.com/watch?v=jNQXAC9IVRw", false, 200, "player"},
		{"player for a collapsed scheme", "/https:/youtu.be/jNQXAC9IVRw", false, 200, "player"},
		{"unknown path", "/nope", false, 404, ""},
		{"admin redirects to the login page", "/admin", false, 302, ""},
		{"login page", "/login", false, 200, "login"},
		{"login page sends signed-in users to admin", "/login", true, 302, ""},
		{"admin with login", "/admin", true, 200, "admin"},
		{"admin has no trailing-slash route", "/admin/", true, 404, ""},
		{"admin.html is not public at the root", "/admin.html", false, 404, ""},
		{"admin.html is not public under /static", "/static/admin.html", false, 404, ""},
		{"static file", "/static/app.js", false, 200, "// app"},
		{"nested static file", "/static/admin/admin.css", false, 200, "body{}"},
		{"service worker at the root", "/admin-sw.js", false, 200, "// sw"},
		{"path traversal", "/static/../admin.html", false, 404, ""},
		{"missing static file", "/static/missing.js", false, 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := app.do(http.MethodGet, tc.target, nil, nil, tc.authed)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.body != "" {
				assert.Contains(t, rec.Body.String(), tc.body)
			}
		})
	}

	assert.Equal(t, "no-cache", app.do(http.MethodGet, "/", nil, nil, false).Header().Get("Cache-Control"))
	assert.Equal(t, "/login", app.do(http.MethodGet, "/admin", nil, nil, false).Header().Get("Location"))
}

func TestLoginAndLogout(t *testing.T) {
	app := newTestApp(t)
	login := func(username, password string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
		return app.do(http.MethodPost, "/api/login", bytes.NewReader(payload), map[string]string{"Content-Type": "application/json"}, false)
	}
	sessionOf := func(rec *httptest.ResponseRecorder) *http.Cookie {
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Name == sessionCookie {
				return cookie
			}
		}
		return nil
	}

	for name, credentials := range map[string][2]string{
		"wrong password": {"admin", "wrong"},
		"wrong username": {"root", "secret"},
		"empty":          {"", ""},
	} {
		rec := login(credentials[0], credentials[1])
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Nil(t, sessionOf(rec), "%s: no cookie", name)
		errorMessage(t, rec)
	}

	rec := login("admin", "secret")
	cookie := sessionOf(rec)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, cookie, "a session cookie")
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.False(t, cookie.Secure, "not Secure over plain HTTP")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/videos", nil)
	req.AddCookie(cookie)
	out := httptest.NewRecorder()
	app.handler.ServeHTTP(out, req)
	assert.Equal(t, http.StatusOK, out.Code, "the admin API accepts the issued cookie")

	secure := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	secure.Header.Set("Content-Type", "application/json")
	secure.Header.Set("X-Forwarded-Proto", "https")
	// Echo only trusts forwarding headers from loopback and private-network peers
	secure.RemoteAddr = "127.0.0.1:50000"
	behindProxy := httptest.NewRecorder()
	app.handler.ServeHTTP(behindProxy, secure)
	require.NotNil(t, sessionOf(behindProxy))
	assert.True(t, sessionOf(behindProxy).Secure, "Secure behind an https proxy")

	rec = app.do(http.MethodPost, "/api/logout", nil, nil, true)
	cleared := sessionOf(rec)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, cleared)
	assert.Empty(t, cleared.Value)
	assert.True(t, cleared.Expires.Before(time.Now()), "the cookie is expired")
}

func TestSessionTokens(t *testing.T) {
	sessions := newSessions(testCreds, testKey)
	token, _ := sessions.issue()
	assert.True(t, sessions.valid(token), "a fresh token is valid")

	expiry, signature, _ := strings.Cut(token, ".")
	other, _ := newSessions(testCreds, []byte("another-key")).issue()
	for name, bad := range map[string]string{
		"empty":        "",
		"no signature": expiry,
		"tampered":     expiry + "." + strings.Repeat("0", len(signature)),
		"later expiry": "99999999999." + signature,
		"not a number": "x." + sessions.sign("x"),
		"other key":    other,
	} {
		assert.False(t, sessions.valid(bad), "%s token is rejected", name)
	}

	later := newSessions(testCreds, testKey)
	later.now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	assert.False(t, later.valid(token), "an expired token is rejected")
}

func TestSessionKeyFromEnvironment(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	t.Setenv("TEST_SESSION_SECRET", "from-env")
	key, err := sessionKeyFromEnvironment("TEST_SESSION_SECRET", logger)
	require.NoError(t, err)
	assert.Equal(t, "from-env", string(key))

	t.Setenv("TEST_SESSION_SECRET", "")
	first, err := sessionKeyFromEnvironment("TEST_SESSION_SECRET", logger)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(first), 32, "a random key is at least 32 bytes")
	second, err := sessionKeyFromEnvironment("TEST_SESSION_SECRET", logger)
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "random keys differ between calls")
}

func TestLoginIsRateLimited(t *testing.T) {
	app := newTestApp(t)
	var last *httptest.ResponseRecorder
	for range 15 {
		last = app.do(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"wrong"}`), map[string]string{"Content-Type": "application/json"}, false)
	}
	assert.Equal(t, http.StatusTooManyRequests, last.Code, "status after 15 failed attempts")
	errorMessage(t, last)
}

func TestVideoAndSubtitleFlow(t *testing.T) {
	app := newTestApp(t)

	assert.Equal(t, 400, app.addVideo("nonsense", "x").Code, "invalid URL")
	assert.Equal(t, 400, app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "  ").Code, "empty title")
	assert.Equal(t, 401, app.do(http.MethodPost, "/api/admin/videos", strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"}, false).Code, "add without login")

	rec := app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "Me at the zoo")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var added struct{ ID int }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &added))

	// the same video in another URL shape is a duplicate
	rec = app.addVideo("https://youtu.be/jNQXAC9IVRw", "again")
	assert.Equal(t, 409, rec.Code)
	assert.Contains(t, errorMessage(t, rec), "already")

	id := strconv.Itoa(added.ID)
	vtt := "WEBVTT\n\n00:01.000 --> 00:02.500\nHello\n\n00:03.000 --> 00:04.000\nWorld\n"
	require.Equal(t, 200, app.upload(id, "TR", "vtt", "a.vtt", vtt).Code, "upload vtt")
	require.Equal(t, 200, app.upload(id, "en", "srt", "a.srt", "\xef\xbb\xbf1\n00:00:01,000 --> 00:00:02,000\nHi\n").Code, "upload srt")
	// uploading a language again replaces it instead of adding a second subtitle (the list check below expects two)
	require.Equal(t, 200, app.upload(id, "en", "srt", "a.srt", "\xef\xbb\xbf1\n00:00:01,000 --> 00:00:02,000\nHi\n").Code, "re-upload srt")
	for name, tc := range map[string]struct {
		rec  *httptest.ResponseRecorder
		want int
	}{
		"bad language":  {app.upload(id, "", "srt", "a.srt", "x"), 400},
		"bad type":      {app.upload(id, "en", "txt", "a.txt", "x"), 400},
		"bad video id":  {app.upload("abc", "en", "srt", "a.srt", "x"), 400},
		"unknown video": {app.upload("999", "en", "srt", "a.srt", "x"), 404},
	} {
		assert.Equal(t, tc.want, tc.rec.Code, "%s: %s", name, tc.rec.Body.String())
	}

	// admin list has subtitles without content
	rec = app.do(http.MethodGet, "/api/admin/videos", nil, nil, true)
	var list []struct {
		Title     string
		Subtitles []Subtitle
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list), rec.Body.String())
	require.Len(t, list, 1)
	require.Len(t, list[0].Subtitles, 2)
	assert.Equal(t, "tr", list[0].Subtitles[0].Language)
	assert.Empty(t, list[0].Subtitles[0].Content)

	// the public API returns content, converted to SRT, and is addressed by any URL shape
	for _, target := range []string{
		"/api/video?url=" + url.QueryEscape("https://www.youtube.com/watch?v=jNQXAC9IVRw"),
		"/api/video?url=" + url.QueryEscape("https://youtu.be/jNQXAC9IVRw"),
	} {
		rec = app.do(http.MethodGet, target, nil, nil, false)
		require.Equal(t, 200, rec.Code, "%s: %s", target, rec.Body.String())
		var video VideoResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &video))
		require.Len(t, video.Subtitles, 2)
		assert.Equal(t, "Me at the zoo", video.Video.Title)
		assert.Equal(t, "jNQXAC9IVRw", video.Video.OriginalURL)
		got := video.Subtitles[0].Content
		assert.True(t, strings.HasPrefix(got, "1\n00,01,000 --> 00,02,500") || strings.HasPrefix(got, "1\n00:01,000 --> 00:02,500"), "converted vtt: %q", got)
		assert.False(t, strings.HasPrefix(video.Subtitles[1].Content, "\xef\xbb\xbf"), "the BOM is stripped")
	}
	rec = app.do(http.MethodGet, "/api/video?url=nope", nil, nil, false)
	assert.Equal(t, 400, rec.Code)
	assert.Equal(t, "Invalid YouTube URL", errorMessage(t, rec))
	assert.Equal(t, 404, app.do(http.MethodGet, "/api/video?url="+url.QueryEscape("https://youtu.be/aaaaaaaaaaa"), nil, nil, false).Code)

	// deleting a video removes its subtitles
	require.Equal(t, 200, app.do(http.MethodDelete, "/api/admin/videos/"+id, nil, nil, true).Code)
	assert.JSONEq(t, "[]", app.do(http.MethodGet, "/api/admin/videos", nil, nil, true).Body.String())
	assert.Equal(t, 400, app.do(http.MethodDelete, "/api/admin/videos/abc", nil, nil, true).Code)
}

func TestRequestLogShowsPathAsText(t *testing.T) {
	app := newTestApp(t)
	app.do(http.MethodGet, "/?v=1", nil, nil, false)
	app.do(http.MethodGet, "/nope", nil, nil, false)

	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(app.logs.String()), "\n") {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "log line is JSON: %q", line)
		entries = append(entries, entry)
	}
	require.Len(t, entries, 2, app.logs.String())
	assert.Equal(t, "/?v=1", entries[0]["path"])
	assert.EqualValues(t, 200, entries[0]["status"])
	assert.Equal(t, "INFO", entries[0]["level"])
	assert.Equal(t, "/nope", entries[1]["path"])
	assert.EqualValues(t, 404, entries[1]["status"])
	assert.Equal(t, "WARN", entries[1]["level"])
}

func TestRequestLogSkipsStaticFiles(t *testing.T) {
	app := newTestApp(t)
	app.do(http.MethodGet, "/static/app.js", nil, nil, false)
	app.do(http.MethodGet, "/admin-sw.js", nil, nil, false)
	assert.Zero(t, app.logs.Len(), "successful static requests are not logged: %s", app.logs.String())

	app.do(http.MethodGet, "/static/missing.js", nil, nil, false)
	assert.Contains(t, app.logs.String(), "/static/missing.js", "a failed static request is logged")
}

func TestDebugEnabled(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "t": true, "0": false, "false": false, "": false, "yes": false, "nonsense": false} {
		t.Setenv("DEBUG", value)
		assert.Equal(t, want, debugEnabled(), "DEBUG=%q", value)
	}
}

func TestCredentialsFromEnvironment(t *testing.T) {
	t.Setenv("ADMIN_CREDENTIALS", "alice:p:ss")
	creds, err := newCredentialsFromEnvironment("ADMIN_CREDENTIALS")
	require.NoError(t, err)
	assert.Equal(t, Credentials{Username: "alice", Password: "p:ss"}, creds)

	for _, bad := range []string{"", "nopassword", ":x", "x:"} {
		t.Setenv("ADMIN_CREDENTIALS", bad)
		_, err := newCredentialsFromEnvironment("ADMIN_CREDENTIALS")
		assert.Error(t, err, "%q should be rejected", bad)
	}
}

func TestListenAddress(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")
	assert.Equal(t, "127.0.0.1:3000", listenAddress(), "default")
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PORT", "8080")
	assert.Equal(t, "0.0.0.0:8080", listenAddress(), "host and port")
	t.Setenv("LISTEN_ADDR", "[::]:9000")
	assert.Equal(t, "[::]:9000", listenAddress(), "listen address")
}

func TestOEmbedProxy(t *testing.T) {
	var lastQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.Query()
		switch lastQuery.Get("url") {
		case "https://www.youtube.com/watch?v=jNQXAC9IVRw":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"title":"  Me at the zoo ","author_name":"jawed","type":"video"}`))
		case "https://www.youtube.com/watch?v=privatevide":
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		case "https://www.youtube.com/watch?v=emptytitle1":
			w.Write([]byte(`{"title":"  "}`))
		case "https://www.youtube.com/watch?v=brokenjson1":
			w.Write([]byte(`not json`))
		case "https://www.youtube.com/watch?v=outage00001":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "Bad Request", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()
	app := newTestAppWith(t, upstream.URL)

	get := func(rawURL string, authed bool) *httptest.ResponseRecorder {
		return app.do(http.MethodGet, "/api/admin/oembed?url="+url.QueryEscape(rawURL), nil, nil, authed)
	}

	rec := get("https://youtu.be/jNQXAC9IVRw", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var result youtube.OEmbedResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, youtube.OEmbedResult{Title: "Me at the zoo", Author: "jawed"}, result)
	// whatever URL shape came in, upstream only ever sees one canonical watch URL
	assert.Equal(t, "json", lastQuery.Get("format"))
	assert.Equal(t, "https://www.youtube.com/watch?v=jNQXAC9IVRw", lastQuery.Get("url"))

	for name, tc := range map[string]struct {
		url    string
		authed bool
		status int
	}{
		"needs login":           {"https://youtu.be/jNQXAC9IVRw", false, 401},
		"not a youtube url":     {"https://example.com/watch?v=jNQXAC9IVRw", true, 400},
		"missing url":           {"", true, 400},
		"too short id":          {"https://youtu.be/abc", true, 400},
		"extra query smuggling": {"https://www.youtube.com/watch?v=jNQXAC9IVRw%26evil%3D1", true, 400},
		"unknown video":         {"https://youtu.be/zzzzzzzzzzz", true, 404},
		"private video":         {"https://youtu.be/privatevide", true, 404},
		"empty title":           {"https://youtu.be/emptytitle1", true, 502},
		"invalid json":          {"https://youtu.be/brokenjson1", true, 502},
		"youtube outage":        {"https://youtu.be/outage00001", true, 502},
	} {
		rec := get(tc.url, tc.authed)
		if assert.Equal(t, tc.status, rec.Code, "%s: %s", name, rec.Body.String()) && tc.authed {
			errorMessage(t, rec)
		}
	}

	upstream.Close()
	rec = get("https://youtu.be/jNQXAC9IVRw", true)
	assert.Equal(t, 502, rec.Code, "upstream down")
	assert.Equal(t, "Couldn't reach YouTube", errorMessage(t, rec))
}

func TestUpdateVideoTitle(t *testing.T) {
	app := newTestApp(t)
	rec := app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "Old title")
	var added struct{ ID int }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &added))
	target := "/api/admin/videos/" + strconv.Itoa(added.ID)
	patch := func(target, body string, authed bool) *httptest.ResponseRecorder {
		return app.do(http.MethodPatch, target, strings.NewReader(body), map[string]string{"Content-Type": "application/json"}, authed)
	}

	assert.Equal(t, 401, patch(target, `{"title":"New title"}`, false).Code, "without login")
	for name, tc := range map[string]struct {
		target, body string
		status       int
	}{
		"empty title":   {target, `{"title":"   "}`, 400},
		"missing title": {target, `{}`, 400},
		"too long":      {target, `{"title":"` + strings.Repeat("x", maxTitleLen+1) + `"}`, 400},
		"bad json":      {target, `nope`, 400},
		"bad id":        {"/api/admin/videos/abc", `{"title":"x"}`, 400},
		"unknown video": {"/api/admin/videos/999", `{"title":"x"}`, 404},
	} {
		rec := patch(tc.target, tc.body, true)
		if assert.Equal(t, tc.status, rec.Code, "%s: %s", name, rec.Body.String()) {
			errorMessage(t, rec)
		}
	}

	rec = patch(target, `{"title":"  Çok güzel video  "}`, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var updated struct {
		ID    int
		Title string
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	assert.Equal(t, added.ID, updated.ID)
	assert.Equal(t, "Çok güzel video", updated.Title)

	// the new title shows in the admin list and in the public API
	rec = app.do(http.MethodGet, "/api/admin/videos", nil, nil, true)
	assert.Contains(t, rec.Body.String(), "Çok güzel video")
	assert.NotContains(t, rec.Body.String(), "Old title")
	rec = app.do(http.MethodGet, "/api/video?url="+url.QueryEscape("https://youtu.be/jNQXAC9IVRw"), nil, nil, false)
	assert.Contains(t, rec.Body.String(), "Çok güzel video")
}
