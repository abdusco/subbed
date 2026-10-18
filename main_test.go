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
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppWith(t, "")
}

func newTestAppWith(t *testing.T, oembedURL string) *testApp {
	t.Helper()
	repo, err := NewRepository(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
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
	e := newServer(serverConfig{repo: repo, assets: assets, creds: testCreds, sessionKey: testKey, logger: logger, oembed: youtube.NewOEmbedClient(nil, oembedURL)})
	return &testApp{t: t, handler: e, logs: logs}
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
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("error Content-Type = %q, want JSON", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] == "" {
		t.Fatalf("error body = %q, want {\"error\": \"...\"}", rec.Body.String())
	}
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
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("body = %q, want it to contain %q", rec.Body.String(), tc.body)
			}
		})
	}

	if got := app.do(http.MethodGet, "/", nil, nil, false).Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control on the page = %q, want no-cache", got)
	}
	if got := app.do(http.MethodGet, "/admin", nil, nil, false).Header().Get("Location"); got != "/login" {
		t.Fatalf("redirect Location = %q, want /login", got)
	}
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
		if rec.Code != http.StatusUnauthorized || sessionOf(rec) != nil {
			t.Fatalf("%s: status = %d, cookie = %v, want 401 and no cookie", name, rec.Code, sessionOf(rec))
		}
		errorMessage(t, rec)
	}

	rec := login("admin", "secret")
	cookie := sessionOf(rec)
	if rec.Code != http.StatusNoContent || cookie == nil {
		t.Fatalf("login status = %d, cookie = %v, want 204 and a session cookie", rec.Code, cookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie should be HttpOnly and SameSite=Lax: %v", cookie)
	}
	if cookie.Secure {
		t.Fatal("session cookie should not be Secure over plain HTTP")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/videos", nil)
	req.AddCookie(cookie)
	out := httptest.NewRecorder()
	app.handler.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("admin API with the issued cookie = %d, want 200", out.Code)
	}

	secure := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	secure.Header.Set("Content-Type", "application/json")
	secure.Header.Set("X-Forwarded-Proto", "https")
	// Echo only trusts forwarding headers from loopback and private-network peers
	secure.RemoteAddr = "127.0.0.1:50000"
	behindProxy := httptest.NewRecorder()
	app.handler.ServeHTTP(behindProxy, secure)
	if cookie := sessionOf(behindProxy); cookie == nil || !cookie.Secure {
		t.Fatalf("cookie behind an https proxy should be Secure: %v", cookie)
	}

	rec = app.do(http.MethodPost, "/api/logout", nil, nil, true)
	if cleared := sessionOf(rec); rec.Code != http.StatusNoContent || cleared == nil || cleared.Value != "" || !cleared.Expires.Before(time.Now()) {
		t.Fatalf("logout status = %d, cookie = %v, want 204 and an expired empty cookie", rec.Code, cleared)
	}
}

func TestSessionTokens(t *testing.T) {
	sessions := newSessions(testCreds, testKey)
	token, _ := sessions.issue()
	if !sessions.valid(token) {
		t.Fatal("a fresh token should be valid")
	}

	expiry, signature, _ := strings.Cut(token, ".")
	for name, bad := range map[string]string{
		"empty":        "",
		"no signature": expiry,
		"tampered":     expiry + "." + strings.Repeat("0", len(signature)),
		"later expiry": "99999999999." + signature,
		"not a number": "x." + sessions.sign("x"),
		"other key": func() string {
			other, _ := newSessions(testCreds, []byte("another-key")).issue()
			return other
		}(),
	} {
		if sessions.valid(bad) {
			t.Fatalf("%s token should be rejected", name)
		}
	}

	later := newSessions(testCreds, testKey)
	later.now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	if later.valid(token) {
		t.Fatal("an expired token should be rejected")
	}
}

func TestSessionKeyFromEnvironment(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	t.Setenv("TEST_SESSION_SECRET", "from-env")
	key, err := sessionKeyFromEnvironment("TEST_SESSION_SECRET", logger)
	if err != nil || string(key) != "from-env" {
		t.Fatalf("key = %q, err = %v, want the env value", key, err)
	}

	t.Setenv("TEST_SESSION_SECRET", "")
	first, err := sessionKeyFromEnvironment("TEST_SESSION_SECRET", logger)
	if err != nil || len(first) < 32 {
		t.Fatalf("random key = %d bytes, err = %v, want at least 32 bytes", len(first), err)
	}
	second, _ := sessionKeyFromEnvironment("TEST_SESSION_SECRET", logger)
	if bytes.Equal(first, second) {
		t.Fatal("random keys should differ between calls")
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	app := newTestApp(t)
	var last *httptest.ResponseRecorder
	for range 15 {
		last = app.do(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"wrong"}`), map[string]string{"Content-Type": "application/json"}, false)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status after 15 failed attempts = %d, want 429", last.Code)
	}
	errorMessage(t, last)
}

func TestVideoAndSubtitleFlow(t *testing.T) {
	app := newTestApp(t)

	if rec := app.addVideo("nonsense", "x"); rec.Code != 400 {
		t.Fatalf("invalid URL: status %d", rec.Code)
	}
	if rec := app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "  "); rec.Code != 400 {
		t.Fatalf("empty title: status %d", rec.Code)
	}
	if rec := app.do(http.MethodPost, "/api/admin/videos", strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"}, false); rec.Code != 401 {
		t.Fatalf("add without login: status %d", rec.Code)
	}

	rec := app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "Me at the zoo")
	if rec.Code != 200 {
		t.Fatalf("add video: status %d body %q", rec.Code, rec.Body.String())
	}
	var added struct{ ID int }
	json.Unmarshal(rec.Body.Bytes(), &added)

	// the same video in another URL shape is a duplicate
	if rec := app.addVideo("https://youtu.be/jNQXAC9IVRw", "again"); rec.Code != 409 || !strings.Contains(errorMessage(t, rec), "already") {
		t.Fatalf("duplicate: status %d body %q", rec.Code, rec.Body.String())
	}

	id := string(rune('0' + added.ID))
	vtt := "WEBVTT\n\n00:01.000 --> 00:02.500\nHello\n\n00:03.000 --> 00:04.000\nWorld\n"
	if rec := app.upload(id, "TR", "vtt", "a.vtt", vtt); rec.Code != 200 {
		t.Fatalf("upload vtt: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := app.upload(id, "en", "srt", "a.srt", "\xef\xbb\xbf1\n00:00:01,000 --> 00:00:02,000\nHi\n"); rec.Code != 200 {
		t.Fatalf("upload srt: status %d", rec.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"bad language":  app.upload(id, "", "srt", "a.srt", "x"),
		"bad type":      app.upload(id, "en", "txt", "a.txt", "x"),
		"bad video id":  app.upload("abc", "en", "srt", "a.srt", "x"),
		"unknown video": app.upload("999", "en", "srt", "a.srt", "x"),
	} {
		want := 400
		if name == "unknown video" {
			want = 404
		}
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d (%q)", name, rec.Code, want, rec.Body.String())
		}
	}

	// admin list has subtitles without content
	rec = app.do(http.MethodGet, "/api/admin/videos", nil, nil, true)
	var list []struct {
		Title     string
		Subtitles []Subtitle
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || len(list[0].Subtitles) != 2 {
		t.Fatalf("list: %v %q", err, rec.Body.String())
	}
	if list[0].Subtitles[0].Language != "tr" || list[0].Subtitles[0].Content != "" {
		t.Fatalf("list subtitle: %+v", list[0].Subtitles[0])
	}

	// the public API returns content, converted to SRT, and is addressed by any URL shape
	for _, target := range []string{
		"/api/video?url=" + url.QueryEscape("https://www.youtube.com/watch?v=jNQXAC9IVRw"),
		"/api/video?url=" + url.QueryEscape("https://youtu.be/jNQXAC9IVRw"),
	} {
		rec = app.do(http.MethodGet, target, nil, nil, false)
		var video VideoResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &video); err != nil || rec.Code != 200 || len(video.Subtitles) != 2 {
			t.Fatalf("video %s: %d %v %q", target, rec.Code, err, rec.Body.String())
		}
		if video.Video.Title != "Me at the zoo" || video.Video.OriginalURL != "jNQXAC9IVRw" {
			t.Fatalf("video: %+v", video.Video)
		}
		if got := video.Subtitles[0].Content; !strings.HasPrefix(got, "1\n00,01,000 --> 00,02,500") && !strings.HasPrefix(got, "1\n00:01,000 --> 00:02,500") {
			t.Fatalf("converted vtt: %q", got)
		}
		if strings.HasPrefix(video.Subtitles[1].Content, "\xef\xbb\xbf") {
			t.Fatal("BOM was not stripped")
		}
	}
	if rec = app.do(http.MethodGet, "/api/video?url=nope", nil, nil, false); rec.Code != 400 || errorMessage(t, rec) != "Invalid YouTube URL" {
		t.Fatalf("invalid public url: %d %q", rec.Code, rec.Body.String())
	}
	if rec = app.do(http.MethodGet, "/api/video?url="+url.QueryEscape("https://youtu.be/aaaaaaaaaaa"), nil, nil, false); rec.Code != 404 {
		t.Fatalf("unknown video: %d", rec.Code)
	}

	// deleting a video removes its subtitles
	if rec = app.do(http.MethodDelete, "/api/admin/videos/"+id, nil, nil, true); rec.Code != 200 {
		t.Fatalf("delete video: %d", rec.Code)
	}
	if rec = app.do(http.MethodGet, "/api/admin/videos", nil, nil, true); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list after delete: %q", rec.Body.String())
	}
	if rec = app.do(http.MethodDelete, "/api/admin/videos/abc", nil, nil, true); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestRequestLogShowsPathAsText(t *testing.T) {
	app := newTestApp(t)
	app.do(http.MethodGet, "/?v=1", nil, nil, false)
	app.do(http.MethodGet, "/nope", nil, nil, false)

	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(app.logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		entries = append(entries, entry)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d log entries, want 2: %s", len(entries), app.logs.String())
	}
	if entries[0]["path"] != "/?v=1" || entries[0]["status"] != float64(200) || entries[0]["level"] != "INFO" {
		t.Fatalf("first entry: %v", entries[0])
	}
	if entries[1]["path"] != "/nope" || entries[1]["status"] != float64(404) || entries[1]["level"] != "WARN" {
		t.Fatalf("second entry: %v", entries[1])
	}
}

func TestRequestLogSkipsStaticFiles(t *testing.T) {
	app := newTestApp(t)
	app.do(http.MethodGet, "/static/app.js", nil, nil, false)
	app.do(http.MethodGet, "/admin-sw.js", nil, nil, false)
	if app.logs.Len() != 0 {
		t.Fatalf("successful static requests were logged: %s", app.logs.String())
	}

	app.do(http.MethodGet, "/static/missing.js", nil, nil, false)
	if !strings.Contains(app.logs.String(), "/static/missing.js") {
		t.Fatalf("a failed static request should be logged: %q", app.logs.String())
	}
}

func TestDebugEnabled(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "t": true, "0": false, "false": false, "": false, "yes": false, "nonsense": false} {
		t.Setenv("DEBUG", value)
		if got := debugEnabled(); got != want {
			t.Errorf("DEBUG=%q: got %v, want %v", value, got, want)
		}
	}
}

func TestCredentialsFromEnvironment(t *testing.T) {
	t.Setenv("ADMIN_CREDENTIALS", "alice:p:ss")
	creds, err := newCredentialsFromEnvironment("ADMIN_CREDENTIALS")
	if err != nil || creds.Username != "alice" || creds.Password != "p:ss" {
		t.Fatalf("got %+v, %v", creds, err)
	}
	for _, bad := range []string{"", "nopassword", ":x", "x:"} {
		t.Setenv("ADMIN_CREDENTIALS", bad)
		if _, err := newCredentialsFromEnvironment("ADMIN_CREDENTIALS"); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestListenAddress(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")
	if got := listenAddress(); got != "127.0.0.1:3000" {
		t.Fatalf("default: %q", got)
	}
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PORT", "8080")
	if got := listenAddress(); got != "0.0.0.0:8080" {
		t.Fatalf("host/port: %q", got)
	}
	t.Setenv("LISTEN_ADDR", "[::]:9000")
	if got := listenAddress(); got != "[::]:9000" {
		t.Fatalf("listen addr: %q", got)
	}
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
	if rec.Code != 200 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	var result youtube.OEmbedResult
	json.Unmarshal(rec.Body.Bytes(), &result)
	if result.Title != "Me at the zoo" || result.Author != "jawed" {
		t.Fatalf("result = %+v", result)
	}
	// whatever URL shape came in, upstream only ever sees one canonical watch URL
	if lastQuery.Get("format") != "json" || lastQuery.Get("url") != "https://www.youtube.com/watch?v=jNQXAC9IVRw" {
		t.Fatalf("upstream query = %v", lastQuery)
	}

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
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d (%q)", name, rec.Code, tc.status, rec.Body.String())
			continue
		}
		if tc.authed {
			errorMessage(t, rec)
		}
	}

	upstream.Close()
	if rec := get("https://youtu.be/jNQXAC9IVRw", true); rec.Code != 502 || errorMessage(t, rec) != "Couldn't reach YouTube" {
		t.Errorf("upstream down: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestUpdateVideoTitle(t *testing.T) {
	app := newTestApp(t)
	rec := app.addVideo("https://www.youtube.com/watch?v=jNQXAC9IVRw", "Old title")
	var added struct{ ID int }
	json.Unmarshal(rec.Body.Bytes(), &added)
	target := "/api/admin/videos/" + strconv.Itoa(added.ID)
	patch := func(target, body string, authed bool) *httptest.ResponseRecorder {
		return app.do(http.MethodPatch, target, strings.NewReader(body), map[string]string{"Content-Type": "application/json"}, authed)
	}

	if rec := patch(target, `{"title":"New title"}`, false); rec.Code != 401 {
		t.Fatalf("without login: status %d", rec.Code)
	}
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
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d (%q)", name, rec.Code, tc.status, rec.Body.String())
			continue
		}
		errorMessage(t, rec)
	}

	rec = patch(target, `{"title":"  Çok güzel video  "}`, true)
	if rec.Code != 200 {
		t.Fatalf("update: status %d body %q", rec.Code, rec.Body.String())
	}
	var updated struct {
		ID    int
		Title string
	}
	json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.ID != added.ID || updated.Title != "Çok güzel video" {
		t.Fatalf("response = %+v", updated)
	}

	// the new title shows in the admin list and in the public API
	rec = app.do(http.MethodGet, "/api/admin/videos", nil, nil, true)
	if !strings.Contains(rec.Body.String(), "Çok güzel video") || strings.Contains(rec.Body.String(), "Old title") {
		t.Fatalf("admin list: %q", rec.Body.String())
	}
	rec = app.do(http.MethodGet, "/api/video?url="+url.QueryEscape("https://youtu.be/jNQXAC9IVRw"), nil, nil, false)
	if !strings.Contains(rec.Body.String(), "Çok güzel video") {
		t.Fatalf("public video: %q", rec.Body.String())
	}
}
