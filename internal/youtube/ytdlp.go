package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultYtDlpBaseURL = "https://github.com/yt-dlp/yt-dlp-nightly-builds/releases/latest/download/"
	// ytDlpUpdateHour is the local hour at which yt-dlp is updated every day
	ytDlpUpdateHour      = 3
	ytDlpDownloadTimeout = 5 * time.Minute
	// killWaitDelay is how long a canceled yt-dlp may take to close its output before it is cut off
	killWaitDelay = 2 * time.Second
)

// YtDlp runs yt-dlp. A yt-dlp found in PATH is preferred and left alone: whoever installed it updates it.
// Otherwise it uses its own copy at path, which is downloaded when missing and kept up to date with nightly
// builds, because YouTube changes often and stable releases lag behind.
type YtDlp struct {
	path    string
	baseURL string
	http    *http.Client
	// mutex keeps an update from replacing the binary while a download is using it
	mutex sync.RWMutex
	// lookPath, now and wait are replaced in tests
	lookPath func(file string) (string, error)
	now      func() time.Time
	wait     func(ctx context.Context, d time.Duration) error
}

// NewYtDlp uses the yt-dlp in PATH if there is one, else manages the binary at path.
func NewYtDlp(path string) *YtDlp {
	return &YtDlp{
		path:     path,
		baseURL:  defaultYtDlpBaseURL,
		http:     &http.Client{Timeout: ytDlpDownloadTimeout},
		lookPath: exec.LookPath,
		now:      time.Now,
		wait: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// ytDlpAssetName is the file of a release that runs on the given platform. musl is true on Alpine and similar.
func ytDlpAssetName(goos, goarch string, musl bool) (string, error) {
	suffix := ""
	switch goarch {
	case "amd64":
	case "arm64":
		suffix = "_aarch64"
	default:
		return "", fmt.Errorf("no yt-dlp build for %s/%s", goos, goarch)
	}
	switch {
	case goos == "darwin":
		return "yt-dlp_macos", nil
	case goos == "linux" && musl:
		return "yt-dlp_musllinux" + suffix, nil
	case goos == "linux":
		return "yt-dlp_linux" + suffix, nil
	}
	return "", fmt.Errorf("no yt-dlp build for %s/%s", goos, goarch)
}

func runsOnMusl() bool {
	matches, _ := filepath.Glob("/lib/ld-musl-*")
	return len(matches) > 0
}

// binary returns the yt-dlp to run, and whether it is the one from PATH.
func (y *YtDlp) binary() (path string, fromPath bool) {
	if found, err := y.lookPath("yt-dlp"); err == nil {
		return found, true
	}
	return y.path, false
}

// Ensure downloads the latest nightly build if there is no yt-dlp in PATH and no copy of our own yet.
func (y *YtDlp) Ensure(ctx context.Context) error {
	y.mutex.Lock()
	defer y.mutex.Unlock()
	return y.ensure(ctx)
}

func (y *YtDlp) ensure(ctx context.Context) error {
	if _, fromPath := y.binary(); fromPath {
		return nil
	}
	if _, err := os.Stat(y.path); err == nil {
		return nil
	}
	asset, err := ytDlpAssetName(runtime.GOOS, runtime.GOARCH, runsOnMusl())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(y.path), 0o755); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, y.baseURL+asset, nil)
	if err != nil {
		return err
	}
	response, err := y.http.Do(request)
	if err != nil {
		return fmt.Errorf("failed to download yt-dlp: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download yt-dlp: status %d", response.StatusCode)
	}

	// write next to the target and rename, so a failed download never leaves a broken binary behind
	partial, err := os.CreateTemp(filepath.Dir(y.path), ".yt-dlp-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(partial.Name())
	if _, err := io.Copy(partial, response.Body); err != nil {
		partial.Close()
		return fmt.Errorf("failed to download yt-dlp: %w", err)
	}
	if err := partial.Close(); err != nil {
		return err
	}
	if err := os.Chmod(partial.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(partial.Name(), y.path)
}

// Version returns the installed version, or an error if yt-dlp is missing or broken.
func (y *YtDlp) Version(ctx context.Context) (string, error) {
	y.mutex.RLock()
	defer y.mutex.RUnlock()
	return y.version(ctx)
}

func (y *YtDlp) version(ctx context.Context) (string, error) {
	path, _ := y.binary()
	output, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("yt-dlp --version failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// Update installs our own yt-dlp if it is missing, then switches it to the latest nightly build. It returns the
// new version and what yt-dlp printed. A yt-dlp from PATH is not touched.
func (y *YtDlp) Update(ctx context.Context) (version, output string, err error) {
	y.mutex.Lock()
	defer y.mutex.Unlock()
	if _, fromPath := y.binary(); fromPath {
		version, err = y.version(ctx)
		return version, "Using the yt-dlp from PATH; it is not updated here", err
	}
	if err := y.ensure(ctx); err != nil {
		return "", "", err
	}
	var combined bytes.Buffer
	command := exec.CommandContext(ctx, y.path, "--update-to", "nightly")
	command.Stdout, command.Stderr = &combined, &combined
	if err := command.Run(); err != nil {
		return "", combined.String(), fmt.Errorf("yt-dlp update failed: %w: %s", err, strings.TrimSpace(combined.String()))
	}
	version, err = y.version(ctx)
	return version, strings.TrimSpace(combined.String()), err
}

// nextRun is the next time the clock shows hour:00 after now.
func nextRun(now time.Time, hour int) time.Time {
	run := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !run.After(now) {
		run = time.Date(now.Year(), now.Month(), now.Day()+1, hour, 0, 0, 0, now.Location())
	}
	return run
}

// RunUpdater updates yt-dlp every day at ytDlpUpdateHour until ctx is done. A failed update is logged and
// tried again the next day.
func (y *YtDlp) RunUpdater(ctx context.Context, logger *slog.Logger) {
	for {
		if err := y.wait(ctx, nextRun(y.now(), ytDlpUpdateHour).Sub(y.now())); err != nil {
			return
		}
		if path, fromPath := y.binary(); fromPath {
			logger.Info("Not updating yt-dlp, using the one from PATH", "path", path)
			continue
		}
		version, _, err := y.Update(ctx)
		if err != nil {
			logger.Error("yt-dlp update failed", "error", err)
			continue
		}
		logger.Info("yt-dlp updated", "version", version)
	}
}

// progressPrefix starts the progress lines that yt-dlp is asked to print, so they can't be mistaken for the file path.
const progressPrefix = "PROGRESS "

// parseProgress reads a "PROGRESS <downloaded> <total> <estimated total>" line, where the sizes are bytes or "NA".
// It returns the fraction that is downloaded; ok is false for any other line or when no total is known.
func parseProgress(line string) (fraction float64, ok bool) {
	fields := strings.Fields(strings.TrimPrefix(line, progressPrefix))
	if !strings.HasPrefix(line, progressPrefix) || len(fields) != 3 {
		return 0, false
	}
	downloaded, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	for _, total := range fields[1:] {
		if size, err := strconv.ParseFloat(total, 64); err == nil && size > 0 {
			return min(downloaded/size, 1), true
		}
	}
	return 0, false
}

// DownloadAudio downloads the best audio of a video into directory and returns the file's path. cookiesFile may be
// empty. progress is called with the fraction downloaded, and what yt-dlp prints besides that goes to logf line by
// line (either may be nil).
func (y *YtDlp) DownloadAudio(ctx context.Context, videoURL, directory, cookiesFile string, progress func(fraction float64), logf func(format string, args ...any)) (string, error) {
	y.mutex.RLock()
	defer y.mutex.RUnlock()

	arguments := []string{
		"--no-playlist", "--newline",
		// --print silences yt-dlp's own progress, so ask for it again in a form that is easy to read
		"--progress", "--progress-template", "download:" + progressPrefix + "%(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s",
		// YouTube serves audio-only streams as Opus or AAC inside a WebM or MP4 container. -x keeps the codec and
		// only gives the file an audio extension (.opus, .m4a); a video that has no audio-only stream is reduced
		// to its audio.
		"--format", "bestaudio/best", "--extract-audio",
		"--output", filepath.Join(directory, "%(id)s.%(ext)s"),
		"--no-simulate", "--print", "after_move:filepath",
	}
	// YouTube needs a JavaScript runtime (yt-dlp only looks for deno by default) and a script that solves its
	// challenges. The standalone builds include the script; a pip install doesn't, so yt-dlp may fetch it from
	// GitHub, as it recommends itself.
	if _, err := y.lookPath("node"); err == nil {
		arguments = append(arguments, "--js-runtimes", "node")
	}
	arguments = append(arguments, "--remote-components", "ejs:github")
	if cookiesFile != "" {
		arguments = append(arguments, "--cookies", cookiesFile)
	}
	arguments = append(arguments, "--", videoURL)

	// stdout holds the progress lines, and the file's path as the last other line
	var path string
	stdout := &lineWriter{emit: func(line string) {
		if strings.HasPrefix(line, progressPrefix) {
			if fraction, ok := parseProgress(line); ok && progress != nil {
				progress(fraction)
			}
			return
		}
		path = line
	}}
	stderr := &lineWriter{emit: func(line string) {
		if logf != nil {
			logf("%s", line)
		}
	}}
	executable, _ := y.binary()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdout, command.Stderr = stdout, stderr
	stopWithChildren(command)
	err := command.Run()
	stdout.Flush()
	stderr.Flush()
	if err != nil {
		return "", fmt.Errorf("yt-dlp failed: %w: %s", err, stderr.Last())
	}
	if path == "" {
		return "", errors.New("yt-dlp did not report the downloaded file")
	}
	return path, nil
}

// lineWriter splits what is written to it into lines, skipping blank ones.
type lineWriter struct {
	emit    func(line string)
	pending string
	last    string
}

func (w *lineWriter) Write(data []byte) (int, error) {
	w.pending += string(data)
	for {
		line, rest, found := strings.Cut(w.pending, "\n")
		if !found {
			return len(data), nil
		}
		w.pending = rest
		w.line(line)
	}
}

func (w *lineWriter) line(text string) {
	if text = strings.TrimSpace(text); text != "" {
		w.last = text
		w.emit(text)
	}
}

// Flush emits an unfinished last line.
func (w *lineWriter) Flush() {
	w.line(w.pending)
	w.pending = ""
}

// Last is the last non-blank line, which is where yt-dlp puts its error message.
func (w *lineWriter) Last() string { return w.last }
