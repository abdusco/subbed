package subtitle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMediaTools writes ffmpeg and ffprobe stand-ins: ffmpeg creates $CHUNKS mp3 chunks next to its last argument
// and ffprobe reports every chunk as 10 seconds long.
func fakeMediaTools(t *testing.T, chunks string) (ffmpeg, ffprobe string) {
	t.Helper()
	directory := t.TempDir()
	ffmpeg = filepath.Join(directory, "ffmpeg")
	ffprobe = filepath.Join(directory, "ffprobe")
	script := "#!/bin/sh\nfor last; do :; done\ndirectory=$(dirname \"$last\")\ni=0\nwhile [ $i -lt $CHUNKS ]; do printf audio > \"$directory/chunk_000$i.mp3\"; i=$((i+1)); done\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\necho 10.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHUNKS", chunks)
	return ffmpeg, ffprobe
}

// newTestTranscriber answers each request with the next of the given handlers' replies and records the payloads.
func newTestTranscriber(t *testing.T, chunks string, replies ...func(w http.ResponseWriter)) (*Transcriber, *[]map[string]any) {
	t.Helper()
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		payloads = append(payloads, payload)
		replies[len(payloads)-1](w)
	}))
	t.Cleanup(server.Close)

	transcriber := NewTranscriber(NewClient("key", server.URL), "stt-model")
	transcriber.FFmpeg, transcriber.FFprobe = fakeMediaTools(t, chunks)
	return transcriber, &payloads
}

func reply(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.Write([]byte(body)) }
}

func TestTranscribeDiarizedChunksAreOffset(t *testing.T) {
	words := `{"language":"en","words":[
		{"word":"Hi","start":0,"end":1,"speaker":"a"},
		{"word":"Hello","start":1.2,"end":2,"speaker":"b"}]}`
	transcriber, payloads := newTestTranscriber(t, "2", reply(words), reply(words))
	transcriber.Language = "en"

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:00,000 --> 00:00:02,000\n- Hi\n- Hello\n\n2\n00:00:10,000 --> 00:00:12,000\n- Hi\n- Hello\n"
	if got.SRT != want || got.Cues != 2 || got.Language != "en" {
		t.Errorf("transcript = %+v, want SRT %q", got, want)
	}
	first := (*payloads)[0]
	if first["model"] != "stt-model" || first["diarize"] != true || first["language"] != "en" || first["response_format"] != "verbose_json" {
		t.Errorf("payload = %v", first)
	}
}

func TestTranscribeUsesDetectedLanguage(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", reply(`{"language":"de","segments":[{"start":0,"end":2,"text":"Hallo"}]}`))
	transcriber.Diarize = false

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	if err != nil || got.Language != "de" || got.SRT != "1\n00:00:00,000 --> 00:00:02,000\nHallo\n" {
		t.Fatalf("transcript = %+v, %v", got, err)
	}
}

func TestTranscribeSpeedUpScalesTimestamps(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", reply(`{"segments":[{"start":0,"end":2,"text":"Hi"}]}`))
	transcriber.Diarize = false
	transcriber.SpeedUp = 2

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	if err != nil || got.SRT != "1\n00:00:00,000 --> 00:00:04,000\nHi\n" {
		t.Fatalf("transcript = %+v, %v", got, err)
	}
}

func TestTranscribeFallsBackWhenTimestampsAreRejected(t *testing.T) {
	rejected := func(w http.ResponseWriter) { http.Error(w, "unsupported response_format", http.StatusBadRequest) }
	transcriber, payloads := newTestTranscriber(t, "1", rejected, reply(`{"text":"One. Two."}`))
	transcriber.Diarize = false

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if want := "1\n00:00:00,000 --> 00:00:05,000\nOne.\n\n2\n00:00:05,000 --> 00:00:10,000\nTwo.\n"; got.SRT != want {
		t.Errorf("SRT = %q, want %q", got.SRT, want)
	}
	if _, kept := (*payloads)[1]["response_format"]; kept || len(*payloads) != 2 {
		t.Errorf("retry payload = %v (%d requests)", (*payloads)[1], len(*payloads))
	}
}

func TestTranscribeFailsWhenDiarizationIsRejected(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", func(w http.ResponseWriter) { http.Error(w, "no diarize", http.StatusBadRequest) })
	if _, err := transcriber.Transcribe(context.Background(), "input.mp4"); err == nil || !strings.Contains(err.Error(), "diarization") {
		t.Fatalf("error = %v", err)
	}
}

func TestTranscribeFailsOnEmptyTranscription(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", reply(`{"text":" ","segments":[],"words":[]}`))
	if _, err := transcriber.Transcribe(context.Background(), "input.mp4"); err == nil || !strings.Contains(err.Error(), "empty transcription") {
		t.Fatalf("error = %v", err)
	}
}

func TestTranscribeFailsWhenFFmpegProducesNothing(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "0")
	if _, err := transcriber.Transcribe(context.Background(), "input.mp4"); err == nil || !strings.Contains(err.Error(), "no audio") {
		t.Fatalf("error = %v", err)
	}
}

func TestTranscribeValidatesSettings(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1")
	transcriber.SpeedUp = 3
	if _, err := transcriber.Transcribe(context.Background(), "input.mp4"); err == nil {
		t.Error("speed-up 3 accepted")
	}
}

func TestSpeakerLabelsMayBeNumbersOrStrings(t *testing.T) {
	var response sttResponse
	body := `{"words":[{"word":"a","speaker":0},{"word":"b","speaker":"speaker_1"},{"word":"c","speaker":null},{"word":"d"}]}`
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	var got []speaker
	for _, word := range response.Words {
		got = append(got, word.Speaker)
	}
	if want := []speaker{"0", "speaker_1", "", ""}; strings.Join(speakerStrings(got), ",") != strings.Join(speakerStrings(want), ",") {
		t.Errorf("speakers = %q, want %q", got, want)
	}
}

func speakerStrings(values []speaker) []string {
	strs := make([]string, len(values))
	for position, value := range values {
		strs[position] = string(value)
	}
	return strs
}
