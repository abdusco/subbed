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
	"strings"
	"testing"
	"testing/fstest"
)

var testCreds = Credentials{Username: "admin", Password: "secret"}

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
		"admin-sw.js":         {Data: []byte("// sw")},
		"app.js":              {Data: []byte("// app")},
		"admin/admin.css":     {Data: []byte("body{}")},
		"admin.manifest.json": {Data: []byte("{}")},
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	e := newServer(serverConfig{repo: repo, assets: assets, creds: testCreds, logger: logger, oembedURL: oembedURL})
	return &testApp{t: t, handler: e, logs: logs}
}

func (a *testApp) do(method, target string, body io.Reader, headers map[string]string, authed bool) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, target, body)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if authed {
		req.SetBasicAuth(testCreds.Username, testCreds.Password)
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
		{"admin needs login", "/admin", false, 401, ""},
		{"admin with login", "/admin", true, 200, "admin"},
		{"admin with trailing slash", "/admin/", true, 200, "admin"},
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
	if got := app.do(http.MethodGet, "/admin", nil, nil, false).Header().Get("WWW-Authenticate"); !strings.Contains(strings.ToLower(got), "basic") {
		t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
	wrong := httptest.NewRequest(http.MethodGet, "/admin", nil)
	wrong.SetBasicAuth("admin", "wrong")
	out := httptest.NewRecorder()
	app.handler.ServeHTTP(out, wrong)
	if out.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", out.Code)
	}
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
	app.do(http.MethodGet, "/static/app.js?v=1", nil, nil, false)
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
	if entries[0]["path"] != "/static/app.js?v=1" || entries[0]["status"] != float64(200) || entries[0]["level"] != "INFO" {
		t.Fatalf("first entry: %v", entries[0])
	}
	if entries[1]["path"] != "/nope" || entries[1]["status"] != float64(404) || entries[1]["level"] != "WARN" {
		t.Fatalf("second entry: %v", entries[1])
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
	var result oembedResult
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
