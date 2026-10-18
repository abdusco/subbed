package youtube

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeYtDlpScript logs its arguments to $FAKE_LOG and answers like yt-dlp for the commands the app uses.
const fakeYtDlpScript = `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$1" in
--version) echo 2026.10.10; exit 0 ;;
--update-to) echo "Updated yt-dlp to nightly@2026.10.10"; exit 0 ;;
esac
if [ -n "$FAKE_FAIL" ]; then
	echo "ERROR: Sign in to confirm you're not a bot" >&2
	exit 1
fi
printf '[youtube] Extracting URL\nWARNING: slow\npartial' >&2
echo "$FAKE_AUDIO"
`

// newTestYtDlp manages the binary at path and ignores whatever yt-dlp or node the machine running the tests has.
func newTestYtDlp(path string) *YtDlp {
	ytdlp := NewYtDlp(path)
	ytdlp.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	return ytdlp
}

// newFakeYtDlp installs the stand-in binary and returns the manager and the file its calls are logged to.
func newFakeYtDlp(t *testing.T) (*YtDlp, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bin", "yt-dlp")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fakeYtDlpScript), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_LOG", log)
	return newTestYtDlp(path), log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestYtDlpAssetName(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
		musl         bool
		want         string
		wantErr      bool
	}{
		{"linux", "amd64", true, "yt-dlp_musllinux", false},
		{"linux", "arm64", true, "yt-dlp_musllinux_aarch64", false},
		{"linux", "amd64", false, "yt-dlp_linux", false},
		{"linux", "arm64", false, "yt-dlp_linux_aarch64", false},
		{"darwin", "arm64", false, "yt-dlp_macos", false},
		{"linux", "riscv64", false, "", true},
		{"windows", "amd64", false, "", true},
	} {
		got, err := ytDlpAssetName(tc.goos, tc.goarch, tc.musl)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("ytDlpAssetName(%s, %s, %v) = %q, %v", tc.goos, tc.goarch, tc.musl, got, err)
		}
	}
}

func TestYtDlpEnsureDownloadsOnlyWhenMissing(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte("#!/bin/sh\necho downloaded\n"))
	}))
	defer server.Close()

	ytdlp := newTestYtDlp(filepath.Join(t.TempDir(), "bin", "yt-dlp"))
	ytdlp.baseURL = server.URL + "/"
	for range 2 {
		if err := ytdlp.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(ytdlp.path)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary: %v %v", info, err)
	}
	if requests.Load() != 1 {
		t.Errorf("%d downloads, want 1", requests.Load())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(ytdlp.path), ".yt-dlp-download-*")); len(leftovers) != 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
}

func TestYtDlpEnsureLeavesNothingBehindOnFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	ytdlp := newTestYtDlp(filepath.Join(t.TempDir(), "bin", "yt-dlp"))
	ytdlp.baseURL = server.URL + "/"
	if err := ytdlp.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(ytdlp.path)); len(entries) != 0 {
		t.Errorf("files left: %v", entries)
	}
}

func TestYtDlpVersionAndUpdate(t *testing.T) {
	ytdlp, log := newFakeYtDlp(t)

	version, err := ytdlp.Version(context.Background())
	if err != nil || version != "2026.10.10" {
		t.Fatalf("version = %q, %v", version, err)
	}
	version, output, err := ytdlp.Update(context.Background())
	if err != nil || version != "2026.10.10" || !strings.Contains(output, "Updated yt-dlp") {
		t.Fatalf("update = %q, %q, %v", version, output, err)
	}
	if !strings.Contains(readLog(t, log), "--update-to nightly") {
		t.Errorf("calls: %q", readLog(t, log))
	}

	missing := newTestYtDlp(filepath.Join(t.TempDir(), "nope"))
	if _, err := missing.Version(context.Background()); err == nil {
		t.Error("missing binary reported a version")
	}
}

func TestYtDlpDownloadAudio(t *testing.T) {
	ytdlp, log := newFakeYtDlp(t)
	t.Setenv("FAKE_AUDIO", "/data/jNQXAC9IVRw.webm")

	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(strings.ReplaceAll(format, "%s", args[0].(string))))
	}
	path, err := ytdlp.DownloadAudio(context.Background(), "https://www.youtube.com/watch?v=jNQXAC9IVRw", t.TempDir(), "/tmp/cookies.txt", logf)
	if err != nil || path != "/data/jNQXAC9IVRw.webm" {
		t.Fatalf("path = %q, %v", path, err)
	}
	called := readLog(t, log)
	for _, want := range []string{"--no-playlist", "--format bestaudio/best", "--cookies /tmp/cookies.txt", "-- https://www.youtube.com/watch?v=jNQXAC9IVRw"} {
		if !strings.Contains(called, want) {
			t.Errorf("arguments %q lack %q", called, want)
		}
	}
	if got := strings.Join(lines, "|"); got != "[youtube] Extracting URL|WARNING: slow|partial" {
		t.Errorf("log lines = %q", got)
	}

	if _, err := ytdlp.DownloadAudio(context.Background(), "https://youtu.be/x", t.TempDir(), "", nil); err != nil {
		t.Fatal(err)
	}
	if lastCall := strings.Split(strings.TrimSpace(readLog(t, log)), "\n"); strings.Contains(lastCall[len(lastCall)-1], "--cookies") {
		t.Errorf("cookies passed without a file: %q", lastCall[len(lastCall)-1])
	}
}

func TestYtDlpDownloadAudioReportsTheError(t *testing.T) {
	ytdlp, _ := newFakeYtDlp(t)
	t.Setenv("FAKE_FAIL", "1")
	_, err := ytdlp.DownloadAudio(context.Background(), "https://youtu.be/x", t.TempDir(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "Sign in to confirm") {
		t.Fatalf("error = %v", err)
	}
}

func TestNextRun(t *testing.T) {
	location := time.FixedZone("test", 3*3600)
	for _, tc := range []struct{ now, want string }{
		{"2026-10-18 01:00", "2026-10-18 03:00"},
		{"2026-10-18 03:00", "2026-10-19 03:00"},
		{"2026-10-18 23:59", "2026-10-19 03:00"},
		{"2026-10-31 12:00", "2026-11-01 03:00"},
	} {
		now, _ := time.ParseInLocation("2006-01-02 15:04", tc.now, location)
		if got := nextRun(now, 3).Format("2006-01-02 15:04"); got != tc.want {
			t.Errorf("nextRun(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestYtDlpUpdaterUpdatesEachRunUntilStopped(t *testing.T) {
	ytdlp, log := newFakeYtDlp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ytdlp.now = func() time.Time { return time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC) }
	waits := 0
	ytdlp.wait = func(ctx context.Context, d time.Duration) error {
		waits++
		if d != 2*time.Hour {
			t.Errorf("waited %v, want 2h", d)
		}
		if waits == 3 {
			cancel()
		}
		return ctx.Err()
	}

	ytdlp.RunUpdater(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := strings.Count(readLog(t, log), "--update-to nightly"); got != 2 {
		t.Errorf("%d updates, want 2", got)
	}
}

// newPathYtDlp returns a manager whose own copy is missing and whose PATH holds the fake yt-dlp and node.
func newPathYtDlp(t *testing.T) (*YtDlp, string) {
	t.Helper()
	fake, log := newFakeYtDlp(t)
	ytdlp := newTestYtDlp(filepath.Join(t.TempDir(), "bin", "yt-dlp"))
	ytdlp.lookPath = func(name string) (string, error) {
		if name == "yt-dlp" {
			return fake.path, nil
		}
		return "/usr/bin/" + name, nil
	}
	return ytdlp, log
}

func TestYtDlpPrefersTheOneInPath(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	ytdlp, log := newPathYtDlp(t)
	ytdlp.baseURL = server.URL + "/"
	t.Setenv("FAKE_AUDIO", "/data/a.webm")

	if err := ytdlp.Ensure(context.Background()); err != nil || requests.Load() != 0 {
		t.Fatalf("Ensure: %v after %d downloads", err, requests.Load())
	}
	if _, err := os.Stat(ytdlp.path); !os.IsNotExist(err) {
		t.Errorf("own copy was created: %v", err)
	}
	if version, err := ytdlp.Version(context.Background()); err != nil || version != "2026.10.10" {
		t.Fatalf("version = %q, %v", version, err)
	}
	if _, err := ytdlp.DownloadAudio(context.Background(), "https://youtu.be/x", t.TempDir(), "", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(t, log), "--js-runtimes node") {
		t.Errorf("node runtime not enabled: %q", readLog(t, log))
	}
}

func TestYtDlpDoesNotUpdateTheOneInPath(t *testing.T) {
	ytdlp, log := newPathYtDlp(t)
	version, output, err := ytdlp.Update(context.Background())
	if err != nil || version != "2026.10.10" || !strings.Contains(output, "from PATH") {
		t.Fatalf("update = %q, %q, %v", version, output, err)
	}
	if strings.Contains(readLog(t, log), "--update-to") {
		t.Errorf("the PATH binary was updated: %q", readLog(t, log))
	}

	// the daily updater leaves it alone as well
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ytdlp.now = func() time.Time { return time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC) }
	waits := 0
	ytdlp.wait = func(ctx context.Context, d time.Duration) error {
		if waits++; waits == 2 {
			cancel()
		}
		return ctx.Err()
	}
	ytdlp.RunUpdater(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if strings.Contains(readLog(t, log), "--update-to") {
		t.Errorf("the updater changed the PATH binary: %q", readLog(t, log))
	}
}
