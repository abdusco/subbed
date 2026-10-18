package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
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
	if err != nil {
		t.Fatal(err)
	}
	want := []Cookie{
		{Domain: ".youtube.com", Name: "SID", Value: "secret-sid", Path: "/", Secure: true, Expires: 1893456000},
		{Domain: ".youtube.com", Name: "__Secure-1PSID", Value: "secret-psid", Path: "/", Secure: true, HTTPOnly: true},
		{Domain: "www.youtube.com", Name: "PREF", Value: "f6=40000000", Path: "/watch", Expires: 1893456000},
		{Domain: ".google.com", Name: "EMPTY", Path: "/", Expires: 1893456000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cookies = %+v\nwant %+v", got, want)
	}
}

func TestParseNetscapeCookiesAcceptsWindowsLineEndings(t *testing.T) {
	got, err := parseNetscapeCookies(strings.ReplaceAll(sampleCookies, "\n", "\r\n"))
	if err != nil || len(got) != 4 || got[0].Value != "secret-sid" {
		t.Fatalf("cookies = %+v, %v", got, err)
	}
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
			if _, err := parseNetscapeCookies(tc.text); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRenderNetscapeCookiesRoundTrips(t *testing.T) {
	cookies, err := parseNetscapeCookies(sampleCookies)
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderNetscapeCookies(cookies)
	if !strings.HasPrefix(rendered, netscapeHeader+"\n") {
		t.Errorf("no header: %q", rendered)
	}
	if !strings.Contains(rendered, "#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\t__Secure-1PSID\tsecret-psid\n") ||
		!strings.Contains(rendered, "www.youtube.com\tFALSE\t/watch\tFALSE\t1893456000\tPREF\tf6=40000000\n") {
		t.Errorf("rendered = %q", rendered)
	}
	again, err := parseNetscapeCookies(rendered)
	if err != nil || !reflect.DeepEqual(again, cookies) {
		t.Fatalf("round trip = %+v, %v", again, err)
	}
}

func TestUpsertCookiesReplacesValuesAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	first, _ := parseNetscapeCookies(sampleCookies)
	if err := repo.UpsertCookies(ctx, first); err != nil {
		t.Fatal(err)
	}

	update := ".youtube.com\tTRUE\t/\tTRUE\t1999999999\tSID\trotated-sid\n.youtube.com\tTRUE\t/\tFALSE\t0\tNEW\tfresh\n"
	second, _ := parseNetscapeCookies(update)
	if err := repo.UpsertCookies(ctx, second); err != nil {
		t.Fatal(err)
	}

	stored, err := repo.ListCookies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Cookie{}
	for _, cookie := range stored {
		byName[cookie.Domain+" "+cookie.Name] = cookie
	}
	if len(stored) != 5 {
		t.Fatalf("%d cookies, want 5: %+v", len(stored), stored)
	}
	if sid := byName[".youtube.com SID"]; sid.Value != "rotated-sid" || sid.Expires != 1999999999 {
		t.Errorf("SID = %+v", sid)
	}
	if psid := byName[".youtube.com __Secure-1PSID"]; psid.Value != "secret-psid" || !psid.HTTPOnly {
		t.Errorf("untouched cookie changed: %+v", psid)
	}
	if byName[".youtube.com NEW"].Value != "fresh" {
		t.Errorf("new cookie missing: %+v", stored)
	}
}

func TestSaveCookiesRejectsTooManyCookies(t *testing.T) {
	var text strings.Builder
	for number := range maxCookies + 1 {
		fmt.Fprintf(&text, ".a.com\tTRUE\t/\tFALSE\t0\tname%d\tv%d\n", number, number)
	}
	app := newTestApp(t)
	rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": text.String()}, true)
	if rec.Code != 400 || errorMessage(t, rec) != "Too many cookies" {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if stored, _ := app.repo.ListCookies(context.Background()); len(stored) != 0 {
		t.Errorf("%d cookies stored", len(stored))
	}
}

func TestWriteCookieFileIsPrivateAndRemovable(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)

	path, cleanup, err := writeCookieFile(ctx, repo)
	if err != nil || path != "" {
		t.Fatalf("without cookies: %q, %v", path, err)
	}
	cleanup()

	cookies, _ := parseNetscapeCookies(sampleCookies)
	if err := repo.UpsertCookies(ctx, cookies); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err = writeCookieFile(ctx, repo)
	if err != nil || path == "" {
		t.Fatalf("with cookies: %q, %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	content, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(content), netscapeHeader) || !strings.Contains(string(content), "secret-sid") {
		t.Errorf("content = %q", content)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still there: %v", err)
	}
}

func (a *testApp) putJSON(path string, payload any, authed bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	return a.do(http.MethodPut, path, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"}, authed)
}

func TestCookieEndpoints(t *testing.T) {
	app := newTestApp(t)

	if rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": sampleCookies}, false); rec.Code != 401 {
		t.Fatalf("put without login: %d", rec.Code)
	}
	if rec := app.do(http.MethodGet, "/api/admin/cookies", nil, nil, false); rec.Code != 401 {
		t.Fatalf("get without login: %d", rec.Code)
	}
	if rec := app.do(http.MethodGet, "/api/admin/cookies", nil, nil, true); strings.TrimSpace(rec.Body.String()) != `{"count":0,"domains":[]}` {
		t.Fatalf("empty: %q", rec.Body.String())
	}

	rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": sampleCookies}, true)
	want := `{"count":4,"domains":["google.com","www.youtube.com","youtube.com"]}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("put: %d %q", rec.Code, rec.Body.String())
	}
	// values never leave the server
	for _, rec := range []*httptest.ResponseRecorder{rec, app.do(http.MethodGet, "/api/admin/cookies", nil, nil, true)} {
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "f6=") {
			t.Errorf("response leaks cookie values: %q", rec.Body.String())
		}
	}

	// submitting again upserts
	update := ".youtube.com\tTRUE\t/\tTRUE\t1893456000\tSID\trotated\n"
	if rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": update}, true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":4`) {
		t.Fatalf("second put: %d %q", rec.Code, rec.Body.String())
	}
	stored, _ := app.repo.ListCookies(context.Background())
	for _, cookie := range stored {
		if cookie.Name == "SID" && cookie.Value != "rotated" {
			t.Errorf("SID = %q, want rotated", cookie.Value)
		}
	}

	for name, payload := range map[string]any{
		"not cookies": map[string]string{"cookies": "hello"},
		"empty":       map[string]string{"cookies": ""},
		"wrong shape": map[string]int{"cookies": 5},
	} {
		if rec := app.putJSON("/api/admin/cookies", payload, true); rec.Code != 400 {
			t.Errorf("%s: status %d, body %q", name, rec.Code, rec.Body.String())
		}
	}
	rec = app.putJSON("/api/admin/cookies", map[string]string{"cookies": "bad line"}, true)
	if message := errorMessage(t, rec); !strings.HasPrefix(message, "Invalid cookies: line 1") {
		t.Errorf("message = %q", message)
	}
	if rec := app.putJSON("/api/admin/cookies", map[string]string{"cookies": strings.Repeat("x", maxCookiesBytes+1)}, true); rec.Code != 413 {
		t.Errorf("oversized: %d", rec.Code)
	}
	if stored, _ := app.repo.ListCookies(context.Background()); len(stored) != 4 {
		t.Errorf("a rejected request changed the cookies: %d", len(stored))
	}

	if rec := app.do(http.MethodDelete, "/api/admin/cookies", nil, nil, false); rec.Code != 401 {
		t.Fatalf("delete without login: %d", rec.Code)
	}
	rec = app.do(http.MethodDelete, "/api/admin/cookies", nil, nil, true)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"count":0,"domains":[]}` {
		t.Fatalf("delete: %d %q", rec.Code, rec.Body.String())
	}
}

func TestSettingsEndpoints(t *testing.T) {
	app := newTestApp(t)

	if rec := app.do(http.MethodGet, "/api/admin/settings", nil, nil, false); rec.Code != 401 {
		t.Fatalf("get without login: %d", rec.Code)
	}
	if rec := app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": "x/y"}, false); rec.Code != 401 {
		t.Fatalf("put without login: %d", rec.Code)
	}

	read := func() settingsResponse {
		var settings settingsResponse
		rec := app.do(http.MethodGet, "/api/admin/settings", nil, nil, true)
		if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil || rec.Code != 200 {
			t.Fatalf("get: %d %q", rec.Code, rec.Body.String())
		}
		return settings
	}
	if settings := read(); settings.TranscribeModel != "" || settings.TranscribeModelDefault != "elevenlabs/scribe-v2" {
		t.Fatalf("defaults = %+v", settings)
	}
	if model, _ := app.repo.transcribeModel(context.Background()); model != "elevenlabs/scribe-v2" {
		t.Fatalf("effective default = %q", model)
	}

	rec := app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": "  openai/whisper-large-v3  "}, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"transcribe_model":"openai/whisper-large-v3"`) {
		t.Fatalf("put: %d %q", rec.Code, rec.Body.String())
	}
	if model, _ := app.repo.transcribeModel(context.Background()); model != "openai/whisper-large-v3" || read().TranscribeModel != model {
		t.Fatalf("effective = %q", model)
	}

	// a request without the field changes nothing
	if rec := app.putJSON("/api/admin/settings", map[string]string{}, true); rec.Code != 200 || read().TranscribeModel != "openai/whisper-large-v3" {
		t.Fatalf("empty put: %d", rec.Code)
	}
	for name, model := range map[string]string{"space inside": "two words", "newline": "a\nb", "too long": strings.Repeat("m", maxModelLen+1)} {
		if rec := app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": model}, true); rec.Code != 400 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}

	// an empty model restores the default
	if rec := app.putJSON("/api/admin/settings", map[string]string{"transcribe_model": " "}, true); rec.Code != 200 || read().TranscribeModel != "" {
		t.Fatalf("reset: %d", rec.Code)
	}
}
