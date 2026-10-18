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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
echo "PROGRESS 100 400 NA"
echo "PROGRESS 400 400 NA"
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
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(fakeYtDlpScript), 0o755))
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_LOG", log)
	return newTestYtDlp(path), log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if !os.IsNotExist(err) {
		require.NoError(t, err)
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
		assert.Equal(t, tc.want, got, "%s/%s musl=%v", tc.goos, tc.goarch, tc.musl)
		assert.Equal(t, tc.wantErr, err != nil, "%s/%s musl=%v: %v", tc.goos, tc.goarch, tc.musl, err)
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
		require.NoError(t, ytdlp.Ensure(context.Background()))
	}

	info, err := os.Stat(ytdlp.path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o100, "the binary is executable")
	assert.EqualValues(t, 1, requests.Load(), "downloads")
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(ytdlp.path), ".yt-dlp-download-*"))
	assert.Empty(t, leftovers, "temporary files")
}

func TestYtDlpEnsureLeavesNothingBehindOnFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	ytdlp := newTestYtDlp(filepath.Join(t.TempDir(), "bin", "yt-dlp"))
	ytdlp.baseURL = server.URL + "/"
	assert.ErrorContains(t, ytdlp.Ensure(context.Background()), "404")
	entries, _ := os.ReadDir(filepath.Dir(ytdlp.path))
	assert.Empty(t, entries)
}

func TestYtDlpVersionAndUpdate(t *testing.T) {
	ytdlp, log := newFakeYtDlp(t)

	version, err := ytdlp.Version(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2026.10.10", version)

	version, output, err := ytdlp.Update(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2026.10.10", version)
	assert.Contains(t, output, "Updated yt-dlp")
	assert.Contains(t, readLog(t, log), "--update-to nightly")

	missing := newTestYtDlp(filepath.Join(t.TempDir(), "nope"))
	_, err = missing.Version(context.Background())
	assert.Error(t, err, "a missing binary has no version")
}

func TestYtDlpDownloadAudio(t *testing.T) {
	ytdlp, log := newFakeYtDlp(t)
	t.Setenv("FAKE_AUDIO", "/data/jNQXAC9IVRw.webm")

	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(strings.ReplaceAll(format, "%s", args[0].(string))))
	}
	var fractions []float64
	progress := func(fraction float64) { fractions = append(fractions, fraction) }
	path, err := ytdlp.DownloadAudio(context.Background(), "https://www.youtube.com/watch?v=jNQXAC9IVRw", t.TempDir(), "/tmp/cookies.txt", progress, logf)
	require.NoError(t, err)
	assert.Equal(t, "/data/jNQXAC9IVRw.webm", path, "the path is the last line that isn't progress")
	assert.Equal(t, []float64{0.25, 1}, fractions)

	called := readLog(t, log)
	for _, want := range []string{"--no-playlist", "--format bestaudio/best", "--extract-audio", "--remote-components ejs:github", "--cookies /tmp/cookies.txt", "-- https://www.youtube.com/watch?v=jNQXAC9IVRw"} {
		assert.Contains(t, called, want)
	}
	assert.Equal(t, []string{"[youtube] Extracting URL", "WARNING: slow", "partial"}, lines)

	_, err = ytdlp.DownloadAudio(context.Background(), "https://youtu.be/x", t.TempDir(), "", nil, nil)
	require.NoError(t, err)
	calls := strings.Split(strings.TrimSpace(readLog(t, log)), "\n")
	assert.NotContains(t, calls[len(calls)-1], "--cookies", "cookies are only passed with a file")
}

// A real yt-dlp starts ffmpeg and a JavaScript runtime. Canceling must stop those as well, and must not wait for
// them to finish by themselves: a child that outlives yt-dlp keeps its output open.
func TestYtDlpDownloadAudioStopsPromptlyWhenCanceledEvenWithChildProcesses(t *testing.T) {
	ytdlp, _ := newFakeYtDlp(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("CHILD_PID_FILE", pidFile)
	slow := `#!/bin/sh
sleep 60 &
echo $! > "$CHILD_PID_FILE"
echo "PROGRESS 10 100 NA"
wait
echo /tmp/never.opus
`
	require.NoError(t, os.WriteFile(ytdlp.path, []byte(slow), 0o755))
	t.Cleanup(func() {
		// whatever the outcome, don't leave the sleeping child behind
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	progress := func(float64) { once.Do(func() { close(started) }) }
	result := make(chan error, 1)
	go func() {
		_, err := ytdlp.DownloadAudio(ctx, "https://youtu.be/x", t.TempDir(), "", progress, nil)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the download never started")
	}
	cancel()
	select {
	case err := <-result:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled download kept waiting for its child process")
	}

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH }, 3*time.Second, 20*time.Millisecond,
		"the child process was left running after the download was canceled")
}

func TestParseProgress(t *testing.T) {
	for _, tc := range []struct {
		line string
		want float64
		ok   bool
	}{
		{"PROGRESS 100 400 NA", 0.25, true},
		{"PROGRESS 400 400 NA", 1, true},
		{"PROGRESS 50 NA 200", 0.25, true},
		{"PROGRESS 500 400 NA", 1, true},
		{"PROGRESS 0 400 NA", 0, true},
		{"PROGRESS 50 NA NA", 0, false},
		{"PROGRESS NA 400 NA", 0, false},
		{"PROGRESS 100 0 NA", 0, false},
		{"PROGRESS 100 400", 0, false},
		{"/data/video.opus", 0, false},
		{"[download] 25.0% of 1MiB", 0, false},
	} {
		got, ok := parseProgress(tc.line)
		assert.Equal(t, tc.ok, ok, tc.line)
		assert.Equal(t, tc.want, got, tc.line)
	}
}

func TestYtDlpDownloadAudioReportsTheError(t *testing.T) {
	ytdlp, _ := newFakeYtDlp(t)
	t.Setenv("FAKE_FAIL", "1")
	_, err := ytdlp.DownloadAudio(context.Background(), "https://youtu.be/x", t.TempDir(), "", nil, nil)
	assert.ErrorContains(t, err, "Sign in to confirm")
}

func TestNextRun(t *testing.T) {
	location := time.FixedZone("test", 3*3600)
	for _, tc := range []struct{ now, want string }{
		{"2026-10-18 01:00", "2026-10-18 03:00"},
		{"2026-10-18 03:00", "2026-10-19 03:00"},
		{"2026-10-18 23:59", "2026-10-19 03:00"},
		{"2026-10-31 12:00", "2026-11-01 03:00"},
	} {
		now, err := time.ParseInLocation("2006-01-02 15:04", tc.now, location)
		require.NoError(t, err)
		assert.Equal(t, tc.want, nextRun(now, 3).Format("2006-01-02 15:04"), "from %s", tc.now)
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
		assert.Equal(t, 2*time.Hour, d)
		if waits == 3 {
			cancel()
		}
		return ctx.Err()
	}

	ytdlp.RunUpdater(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.Equal(t, 2, strings.Count(readLog(t, log), "--update-to nightly"), "updates")
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

	require.NoError(t, ytdlp.Ensure(context.Background()))
	assert.Zero(t, requests.Load(), "downloads")
	_, err := os.Stat(ytdlp.path)
	assert.True(t, os.IsNotExist(err), "own copy was created")

	version, err := ytdlp.Version(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2026.10.10", version)

	_, err = ytdlp.DownloadAudio(context.Background(), "https://youtu.be/x", t.TempDir(), "", nil, nil)
	require.NoError(t, err)
	assert.Contains(t, readLog(t, log), "--js-runtimes node")
}

func TestYtDlpDoesNotUpdateTheOneInPath(t *testing.T) {
	ytdlp, log := newPathYtDlp(t)
	version, output, err := ytdlp.Update(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2026.10.10", version)
	assert.Contains(t, output, "from PATH")
	assert.NotContains(t, readLog(t, log), "--update-to")

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
	assert.NotContains(t, readLog(t, log), "--update-to")
}
