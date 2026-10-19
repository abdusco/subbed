package subtitle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"subbed/internal/logger"
)

const (
	minChunkDuration = 0.1
	// DefaultTranscribeModel is used when no model is configured.
	DefaultTranscribeModel = "openai/whisper-large-v3-turbo"
)

// Transcriber turns an audio or video file into an SRT with an OpenRouter speech-to-text model.
type Transcriber struct {
	Client *Client
	Model  string
	// Language is the ISO-639-1 source language; empty means auto-detect
	Language string
	// Diarize asks for speaker labels; short speaker changes become "- " dialogue cues. A model that doesn't
	// support them is used without them.
	Diarize bool
	// diarizeRejected is set once a model has refused speaker labels during a Transcribe call
	diarizeRejected bool
	// SpeedUp (1.0-2.0) speeds the audio up before transcribing to reduce cost; timestamps are scaled back
	SpeedUp float64
	// ChunkMinutes is the audio chunk length in minutes, measured after the speed-up
	ChunkMinutes float64
	FFmpeg       string
	FFprobe      string
	// Progress is called with how much of the work is done (0 to 1) and what is being done; it may be nil
	Progress func(fraction float64, stage string)
}

// Transcript is the result of a transcription.
type Transcript struct {
	// SRT is the subtitle text
	SRT string
	// Language is the source language: the configured one, else what the model detected (may be empty)
	Language string
	Cues     int
}

// NewTranscriber returns a Transcriber with the defaults: diarization on, no speed-up, 60 minute chunks.
func NewTranscriber(client *Client, model string) *Transcriber {
	return &Transcriber{
		Client:       client,
		Model:        model,
		Diarize:      true,
		SpeedUp:      1.0,
		ChunkMinutes: 60,
		FFmpeg:       "ffmpeg",
		FFprobe:      "ffprobe",
	}
}

func (t *Transcriber) progress(fraction float64, stage string) {
	if t.Progress != nil {
		t.Progress(fraction, stage)
	}
}

// sttWord and sttResponse are the parts of the transcription response that are used.
type sttWord struct {
	Word    string  `json:"word"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Type    string  `json:"type"`
	Speaker speaker `json:"speaker"`
}

type sttResponse struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	Segments []struct {
		Start   float64 `json:"start"`
		End     float64 `json:"end"`
		Text    string  `json:"text"`
		Speaker speaker `json:"speaker"`
	} `json:"segments"`
	Words []sttWord `json:"words"`
}

// speaker is a speaker label that models send as a number or a string; null or missing is "".
type speaker string

func (s *speaker) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch value := value.(type) {
	case nil:
		*s = ""
	case string:
		*s = speaker(value)
	case float64:
		*s = speaker(strconv.FormatFloat(value, 'f', -1, 64))
	default:
		return fmt.Errorf("unsupported speaker label %v", value)
	}
	return nil
}

// Transcribe extracts the audio of path, transcribes it chunk by chunk and returns the SRT.
func (t *Transcriber) Transcribe(ctx context.Context, path string) (Transcript, error) {
	if t.Client == nil || t.Model == "" {
		return Transcript{}, errors.New("transcriber needs a client and a model")
	}
	if t.SpeedUp < 1 || t.SpeedUp > 2 {
		return Transcript{}, errors.New("speed-up must be between 1.0 and 2.0")
	}
	if t.ChunkMinutes <= 0 {
		return Transcript{}, errors.New("chunk length must be positive")
	}

	directory, err := os.MkdirTemp("", "subbed-audio-")
	if err != nil {
		return Transcript{}, err
	}
	defer os.RemoveAll(directory)

	chunks, err := t.extractChunks(ctx, path, directory)
	if err != nil {
		return Transcript{}, err
	}
	logger.FromCtx(ctx).Info(fmt.Sprintf("Extracted %d audio chunk(s)", len(chunks)))

	// cutting the audio is quick next to transcribing it
	const extractionShare = 0.05
	t.diarizeRejected = false
	language := t.Language
	var segments []Segment
	offset := 0.0
	for position, chunk := range chunks {
		duration, err := t.duration(ctx, chunk)
		if err != nil {
			return Transcript{}, err
		}
		if duration < minChunkDuration {
			logger.FromCtx(ctx).Info(fmt.Sprintf("Skipping empty chunk %d/%d", position+1, len(chunks)))
			continue
		}
		logger.FromCtx(ctx).Debug(fmt.Sprintf("Transcribing chunk %d/%d with %s", position+1, len(chunks), t.Model))
		t.progress(extractionShare+(1-extractionShare)*float64(position)/float64(len(chunks)), fmt.Sprintf("Transcribing chunk %d/%d", position+1, len(chunks)))
		result, err := t.transcribeChunk(ctx, chunk, duration)
		if err != nil {
			return Transcript{}, fmt.Errorf("chunk %d/%d: %w", position+1, len(chunks), err)
		}
		if language == "" {
			language = result.language
		}
		for _, segment := range result.segments {
			start := segment.Start*t.SpeedUp + offset
			end := segment.End*t.SpeedUp + offset
			pieces := []timedText{{start, end, segment.Text}}
			if t.SpeedUp > 1 || t.diarizing() || !result.hasTimestamps {
				pieces = splitCue(start, end, segment.Text)
			}
			for _, piece := range pieces {
				segments = append(segments, Segment{Start: piece.start, End: piece.end, Text: piece.text, Speaker: segment.Speaker})
			}
		}
		offset += duration * t.SpeedUp
	}
	if len(segments) == 0 {
		return Transcript{}, errors.New("transcription returned no text")
	}
	t.progress(1, "")

	cues := buildCues(formatCues(groupDialogue(segments)))
	return Transcript{SRT: RenderSRT(cues), Language: language, Cues: len(cues)}, nil
}

// extractChunks writes mono 16 kHz mp3 chunks of the audio into directory, in order.
func (t *Transcriber) extractChunks(ctx context.Context, path, directory string) ([]string, error) {
	arguments := []string{"-hide_banner", "-loglevel", "error", "-i", path, "-vn", "-ac", "1", "-ar", "16000"}
	if t.SpeedUp != 1 {
		arguments = append(arguments, "-filter:a", "atempo="+strconv.FormatFloat(t.SpeedUp, 'f', -1, 64))
	}
	arguments = append(arguments,
		"-c:a", "libmp3lame", "-b:a", "48k",
		"-f", "segment", "-segment_time", strconv.Itoa(int(t.ChunkMinutes*60)), "-reset_timestamps", "1",
		filepath.Join(directory, "chunk_%04d.mp3"),
	)
	if output, err := exec.CommandContext(ctx, t.FFmpeg, arguments...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	chunks, err := filepath.Glob(filepath.Join(directory, "chunk_*.mp3"))
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no audio from %s", path)
	}
	return chunks, nil // Glob returns sorted names
}

// duration returns the length of a media file in seconds via ffprobe, or 0 if it is unreadable
// (an empty trailing chunk, for example).
func (t *Transcriber) duration(ctx context.Context, path string) (float64, error) {
	output, err := exec.CommandContext(ctx, t.FFprobe,
		"-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w", err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return 0, nil
	}
	return max(seconds, 0), nil
}

type chunkResult struct {
	// segments are timed relative to the chunk, in sped-up audio time
	segments      []Segment
	hasTimestamps bool
	language      string
}

// diarizing reports whether speaker labels are being asked for: they are wanted, and no model has refused them yet.
func (t *Transcriber) diarizing() bool {
	return t.Diarize && !t.diarizeRejected
}

// chunkRequest is the transcription request for one chunk. A plain request asks for no timestamps, for models that
// don't return any.
func (t *Transcriber) chunkRequest(audio []byte, plain bool) map[string]any {
	payload := map[string]any{
		"model":       t.Model,
		"input_audio": map[string]string{"data": base64.StdEncoding.EncodeToString(audio), "format": "mp3"},
	}
	if !plain {
		payload["response_format"] = "verbose_json"
		payload["timestamp_granularities"] = []string{"segment"}
	}
	if t.Language != "" {
		payload["language"] = t.Language
	}
	if t.diarizing() {
		payload["diarize"] = true
		if !plain {
			payload["timestamp_granularities"] = []string{"segment", "word"}
		}
	}
	return payload
}

// transcribeChunk transcribes one audio chunk. If the model refuses speaker labels, the request is repeated without
// them, and so are the following chunks. If it then rejects verbose_json, the request is repeated as plain JSON and
// one segment covers the whole chunk (hasTimestamps false).
// With speaker labels, turns are built from word labels, falling back to segment labels.
func (t *Transcriber) transcribeChunk(ctx context.Context, path string, duration float64) (chunkResult, error) {
	audio, err := os.ReadFile(path)
	if err != nil {
		return chunkResult{}, err
	}

	response, err := t.Client.Post(ctx, "/audio/transcriptions", t.chunkRequest(audio, false))
	if err != nil {
		return chunkResult{}, err
	}
	if response.Status == 400 && t.diarizing() {
		logger.FromCtx(ctx).Warn(fmt.Sprintf("The model doesn't take speaker labels (%.200s); continuing without them", response.Body))
		t.diarizeRejected = true
		if response, err = t.Client.Post(ctx, "/audio/transcriptions", t.chunkRequest(audio, false)); err != nil {
			return chunkResult{}, err
		}
	}
	if response.Status == 400 {
		logger.FromCtx(ctx).Warn(fmt.Sprintf("Model rejected verbose_json (%.200s); using a single cue per chunk", response.Body))
		if response, err = t.Client.Post(ctx, "/audio/transcriptions", t.chunkRequest(audio, true)); err != nil {
			return chunkResult{}, err
		}
	}
	if response.Status != 200 {
		return chunkResult{}, fmt.Errorf("transcription failed with status %d: %.300s", response.Status, response.Body)
	}

	var data sttResponse
	if err := json.Unmarshal(response.Body, &data); err != nil {
		return chunkResult{}, fmt.Errorf("unreadable transcription response: %w", err)
	}
	if len(data.Segments) == 0 && len(data.Words) == 0 && strings.TrimSpace(data.Text) == "" {
		return chunkResult{}, fmt.Errorf("empty transcription (%d bytes of audio); response: %.300s", len(audio), response.Body)
	}

	result := chunkResult{language: data.Language, hasTimestamps: true}
	if t.diarizing() {
		if turns := turnsFromWords(data.Words); len(turns) > 0 {
			result.segments = turns
			return result, nil
		}
	}
	labelled := false
	for _, segment := range data.Segments {
		text := strings.TrimSpace(segment.Text)
		if text == "" {
			continue
		}
		label := ""
		if t.diarizing() {
			label = string(segment.Speaker)
		}
		labelled = labelled || label != ""
		result.segments = append(result.segments, Segment{Start: segment.Start, End: segment.End, Text: text, Speaker: label})
	}
	if t.diarizing() && !labelled {
		logger.FromCtx(ctx).Warn("No speaker labels in the response; the model may not support them")
	}
	if len(result.segments) > 0 {
		return result, nil
	}
	result.hasTimestamps = false
	if text := strings.TrimSpace(data.Text); text != "" {
		result.segments = []Segment{{Start: 0, End: duration, Text: text}}
	}
	return result, nil
}
