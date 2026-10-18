package subtitle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMediaTools writes ffmpeg and ffprobe stand-ins: ffmpeg creates $CHUNKS mp3 chunks next to its last argument
// and ffprobe reports every chunk as 10 seconds long.
func fakeMediaTools(t *testing.T, chunks string) (ffmpeg, ffprobe string) {
	t.Helper()
	directory := t.TempDir()
	ffmpeg = filepath.Join(directory, "ffmpeg")
	ffprobe = filepath.Join(directory, "ffprobe")
	script := "#!/bin/sh\nfor last; do :; done\ndirectory=$(dirname \"$last\")\ni=0\nwhile [ $i -lt $CHUNKS ]; do printf audio > \"$directory/chunk_000$i.mp3\"; i=$((i+1)); done\n"
	require.NoError(t, os.WriteFile(ffmpeg, []byte(script), 0o755))
	require.NoError(t, os.WriteFile(ffprobe, []byte("#!/bin/sh\necho 10.0\n"), 0o755))
	t.Setenv("CHUNKS", chunks)
	return ffmpeg, ffprobe
}

// newTestTranscriber answers each request with the next of the given replies and records the payloads.
func newTestTranscriber(t *testing.T, chunks string, replies ...func(w http.ResponseWriter)) (*Transcriber, *[]map[string]any) {
	t.Helper()
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
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
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:02,000\n- Hi\n- Hello\n\n2\n00:00:10,000 --> 00:00:12,000\n- Hi\n- Hello\n", got.SRT)
	assert.Equal(t, 2, got.Cues)
	assert.Equal(t, "en", got.Language)

	require.NotEmpty(t, *payloads)
	first := (*payloads)[0]
	assert.Equal(t, "stt-model", first["model"])
	assert.Equal(t, true, first["diarize"])
	assert.Equal(t, "en", first["language"])
	assert.Equal(t, "verbose_json", first["response_format"])
}

func TestTranscribeReportsProgressChunkByChunk(t *testing.T) {
	segments := reply(`{"segments":[{"start":0,"end":2,"text":"Hi"}]}`)
	transcriber, _ := newTestTranscriber(t, "4", segments, segments, segments, segments)
	transcriber.Diarize = false
	type report struct {
		fraction float64
		stage    string
	}
	var reports []report
	transcriber.Progress = func(fraction float64, stage string) { reports = append(reports, report{fraction, stage}) }

	_, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)

	require.Len(t, reports, 5)
	for position, want := range []string{"Transcribing chunk 1/4", "Transcribing chunk 2/4", "Transcribing chunk 3/4", "Transcribing chunk 4/4", ""} {
		assert.Equal(t, want, reports[position].stage)
	}
	assert.InDelta(t, 0.05, reports[0].fraction, 1e-9, "the first chunk starts after the audio is cut")
	assert.Equal(t, 1.0, reports[4].fraction)
	for position := 1; position < len(reports); position++ {
		assert.Greater(t, reports[position].fraction, reports[position-1].fraction, "progress only moves forward")
	}
}

func TestTranscribeUsesDetectedLanguage(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", reply(`{"language":"de","segments":[{"start":0,"end":2,"text":"Hallo"}]}`))
	transcriber.Diarize = false

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	assert.Equal(t, "de", got.Language)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:02,000\nHallo\n", got.SRT)
}

func TestTranscribeSpeedUpScalesTimestamps(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", reply(`{"segments":[{"start":0,"end":2,"text":"Hi"}]}`))
	transcriber.Diarize = false
	transcriber.SpeedUp = 2

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:04,000\nHi\n", got.SRT)
}

func TestTranscribeFallsBackWhenTimestampsAreRejected(t *testing.T) {
	rejected := func(w http.ResponseWriter) { http.Error(w, "unsupported response_format", http.StatusBadRequest) }
	transcriber, payloads := newTestTranscriber(t, "1", rejected, reply(`{"text":"One. Two."}`))
	transcriber.Diarize = false

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:05,000\nOne.\n\n2\n00:00:05,000 --> 00:00:10,000\nTwo.\n", got.SRT)
	require.Len(t, *payloads, 2)
	assert.NotContains(t, (*payloads)[1], "response_format", "the retry asks for plain JSON")
}

func TestTranscribeContinuesWithoutSpeakerLabelsWhenTheModelRefusesThem(t *testing.T) {
	refused := func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"The selected model does not support diarize."}}`, http.StatusBadRequest)
	}
	segments := reply(`{"language":"en","segments":[{"start":0,"end":2,"text":"Hello there."}]}`)
	transcriber, payloads := newTestTranscriber(t, "2", refused, segments, segments)
	var logged []string
	transcriber.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:02,000\nHello there.\n\n2\n00:00:10,000 --> 00:00:12,000\nHello there.\n", got.SRT)
	assert.Equal(t, "en", got.Language)

	require.Len(t, *payloads, 3, "the refused request is repeated once, and the next chunk doesn't ask again")
	assert.Equal(t, true, (*payloads)[0]["diarize"])
	assert.NotContains(t, (*payloads)[1], "diarize")
	assert.Equal(t, []any{"segment"}, (*payloads)[1]["timestamp_granularities"], "no word timestamps without speaker labels")
	assert.NotContains(t, (*payloads)[2], "diarize")
	var notices int
	for _, line := range logged {
		if strings.Contains(line, "speaker labels") {
			notices++
		}
	}
	assert.Equal(t, 1, notices, "the refusal is reported once: %q", logged)
}

func TestTranscribeKeepsSpeakerLabelsWhenTheModelSupportsThem(t *testing.T) {
	words := reply(`{"language":"en","words":[{"word":"Hi","start":0,"end":1,"speaker":"a"},{"word":"Hello","start":1.2,"end":2,"speaker":"b"}]}`)
	transcriber, payloads := newTestTranscriber(t, "1", words)

	got, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	assert.Contains(t, got.SRT, "- Hi\n- Hello")
	require.Len(t, *payloads, 1)
	assert.Equal(t, true, (*payloads)[0]["diarize"])
}

func TestTranscribeRefusedSpeakerLabelsDoNotLeakIntoTheNextTranscription(t *testing.T) {
	refused := func(w http.ResponseWriter) { http.Error(w, "no diarize", http.StatusBadRequest) }
	segments := reply(`{"segments":[{"start":0,"end":2,"text":"Hi"}]}`)
	words := reply(`{"words":[{"word":"Hi","start":0,"end":1,"speaker":"a"}]}`)
	transcriber, payloads := newTestTranscriber(t, "1", refused, segments, words)

	_, err := transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	_, err = transcriber.Transcribe(context.Background(), "input.mp4")
	require.NoError(t, err)
	require.Len(t, *payloads, 3)
	assert.Equal(t, true, (*payloads)[2]["diarize"], "a new transcription asks again")
}

func TestTranscribeFailsOnEmptyTranscription(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1", reply(`{"text":" ","segments":[],"words":[]}`))
	_, err := transcriber.Transcribe(context.Background(), "input.mp4")
	assert.ErrorContains(t, err, "empty transcription")
}

func TestTranscribeFailsWhenFFmpegProducesNothing(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "0")
	_, err := transcriber.Transcribe(context.Background(), "input.mp4")
	assert.ErrorContains(t, err, "no audio")
}

func TestTranscribeValidatesSettings(t *testing.T) {
	transcriber, _ := newTestTranscriber(t, "1")
	transcriber.SpeedUp = 3
	_, err := transcriber.Transcribe(context.Background(), "input.mp4")
	assert.Error(t, err, "a speed-up of 3 is out of range")
}

func TestSpeakerLabelsMayBeNumbersOrStrings(t *testing.T) {
	var response sttResponse
	body := `{"words":[{"word":"a","speaker":0},{"word":"b","speaker":"speaker_1"},{"word":"c","speaker":null},{"word":"d"}]}`
	require.NoError(t, json.Unmarshal([]byte(body), &response))
	var got []speaker
	for _, word := range response.Words {
		got = append(got, word.Speaker)
	}
	assert.Equal(t, []speaker{"0", "speaker_1", "", ""}, got)
}
