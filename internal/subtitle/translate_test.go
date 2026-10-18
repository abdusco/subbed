package subtitle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatRequest is what a translation request carries, extracted from the prompt.
type chatRequest struct {
	Prompt string
	Before []string
	Items  []struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	}
	After []string
}

func decodeChatRequest(t *testing.T, r *http.Request) chatRequest {
	t.Helper()
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) || !assert.Len(t, body.Messages, 1) {
		return chatRequest{}
	}
	prompt := body.Messages[0].Content
	_, input, _ := strings.Cut(prompt, "Input:\n")
	input, _, _ = strings.Cut(input, "\n\nYour previous answer")
	var parsed struct {
		Before []string `json:"context_before"`
		Items  []struct {
			Index int    `json:"index"`
			Text  string `json:"text"`
		} `json:"items"`
		After []string `json:"context_after"`
	}
	assert.NoError(t, json.Unmarshal([]byte(input), &parsed), "the prompt input is JSON: %s", input)
	return chatRequest{Prompt: prompt, Before: parsed.Before, Items: parsed.Items, After: parsed.After}
}

func completion(content string) string {
	encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	return string(encoded)
}

// translationsFor answers with "<prefix>text" for every item, in the documented JSON shape.
func translationsFor(request chatRequest, prefix string) string {
	var entries []string
	for _, item := range request.Items {
		text, _ := json.Marshal(prefix + item.Text)
		entries = append(entries, fmt.Sprintf(`{"index":%d,"text":%s}`, item.Index, text))
	}
	return completion(`{"translations":[` + strings.Join(entries, ",") + `]}`)
}

func newTestTranslator(t *testing.T, handler func(request chatRequest, call int, w http.ResponseWriter)) *Translator {
	t.Helper()
	var (
		mutex sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeChatRequest(t, r)
		mutex.Lock()
		calls++
		call := calls
		mutex.Unlock()
		handler(request, call, w)
	}))
	t.Cleanup(server.Close)
	client := NewClient("key", server.URL)
	client.backoff = func(int) time.Duration { return 0 }
	return NewTranslator(client, "chat-model")
}

func testCues(count int) []Cue {
	cues := make([]Cue, count)
	for position := range cues {
		cues[position] = Cue{Index: position + 1, Timeline: fmt.Sprintf("00:00:%02d,000 --> 00:00:%02d,500", position, position), Text: fmt.Sprintf("line %d", position+1)}
	}
	return cues
}

func TestTranslateBatchesWithContextAndKeepsTimelines(t *testing.T) {
	var mutex sync.Mutex
	seen := map[string]chatRequest{}
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		mutex.Lock()
		seen[request.Items[0].Text] = request
		mutex.Unlock()
		w.Write([]byte(translationsFor(request, "tr:")))
	})
	translator.BatchSize = 2

	cues := testCues(5)
	got, err := translator.Translate(context.Background(), cues, "Turkish")
	require.NoError(t, err)
	require.Len(t, got, 5)
	for position, cue := range got {
		assert.Equal(t, cues[position].Index, cue.Index)
		assert.Equal(t, cues[position].Timeline, cue.Timeline)
		assert.Equal(t, "tr:"+cues[position].Text, cue.Text)
	}

	require.Len(t, seen, 3, "requests")
	middle := seen["line 3"]
	assert.Equal(t, []string{"line 1", "line 2"}, middle.Before)
	assert.Equal(t, []string{"line 5"}, middle.After)
	assert.Len(t, middle.Items, 2)
	first := seen["line 1"]
	assert.Empty(t, first.Before)
	assert.Contains(t, first.Prompt, "into Turkish.")
}

func TestTranslateReportsProgressPerBatch(t *testing.T) {
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		w.Write([]byte(translationsFor(request, "tr:")))
	})
	translator.BatchSize = 2
	var mutex sync.Mutex
	var fractions []float64
	translator.Progress = func(fraction float64) {
		mutex.Lock()
		defer mutex.Unlock()
		fractions = append(fractions, fraction)
	}

	_, err := translator.Translate(context.Background(), testCues(5), "Turkish")
	require.NoError(t, err)
	assert.ElementsMatch(t, []float64{1.0 / 3, 2.0 / 3, 1}, fractions, "one report per batch")
}

func TestTranslateRetriesAnInvalidReplyWithTheProblem(t *testing.T) {
	var prompts []string
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		prompts = append(prompts, request.Prompt)
		if call == 1 {
			w.Write([]byte(completion(`{"translations":[{"index":1,"text":"only one"}]}`)))
			return
		}
		w.Write([]byte(translationsFor(request, "ok:")))
	})

	got, err := translator.Translate(context.Background(), testCues(2), "German")
	require.NoError(t, err)
	assert.Equal(t, "ok:line 1", got[0].Text)
	require.Len(t, prompts, 2)
	assert.NotContains(t, prompts[0], "previous answer")
	assert.Contains(t, prompts[1], "Your previous answer was invalid: expected 2 translations, got 1")
}

func TestTranslateGivesUpAfterRepeatedInvalidReplies(t *testing.T) {
	var calls int
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		calls++
		w.Write([]byte(completion("I cannot do that")))
	})

	_, err := translator.Translate(context.Background(), testCues(3), "German")
	assert.ErrorContains(t, err, "batch starting at cue 1")
	assert.Equal(t, maxValidationAttempts, calls)
}

func TestTranslateStopsOnClientErrors(t *testing.T) {
	var calls int
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		calls++
		http.Error(w, "bad key", http.StatusUnauthorized)
	})

	_, err := translator.Translate(context.Background(), testCues(2), "German")
	assert.ErrorContains(t, err, "401")
	assert.Equal(t, 1, calls)
}

func TestTranslateKeepsCharactersUnescaped(t *testing.T) {
	var prompt string
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		prompt = request.Prompt
		w.Write([]byte(translationsFor(request, "")))
	})
	cues := []Cue{{Index: 1, Timeline: "00:00:01,000 --> 00:00:02,000", Text: "Tom & Jerry <3 çay"}}
	_, err := translator.Translate(context.Background(), cues, "Turkish")
	require.NoError(t, err)
	assert.Contains(t, prompt, "Tom & Jerry <3 çay")
}

func TestParseTranslations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		want    []string
		wantErr string
	}{
		{name: "plain", reply: `{"translations":[{"index":1,"text":"a"},{"index":2,"text":"b"}]}`, want: []string{"a", "b"}},
		{name: "markdown fence and shuffled order", reply: "```json\n{\"translations\":[{\"index\":2,\"text\":\" b \"},{\"index\":1,\"text\":\"a\"}]}\n```", want: []string{"a", "b"}},
		{name: "no json", reply: "sorry", wantErr: "no JSON object"},
		{name: "wrong count", reply: `{"translations":[{"index":1,"text":"a"}]}`, wantErr: "expected 2 translations, got 1"},
		{name: "duplicate index", reply: `{"translations":[{"index":1,"text":"a"},{"index":1,"text":"b"}]}`, wantErr: "duplicate index 1"},
		{name: "index out of range", reply: `{"translations":[{"index":1,"text":"a"},{"index":3,"text":"b"}]}`, wantErr: "invalid or duplicate index 3"},
		{name: "missing index", reply: `{"translations":[{"index":1,"text":"a"},{"text":"b"}]}`, wantErr: "without an index"},
		{name: "empty text", reply: `{"translations":[{"index":1,"text":"a"},{"index":2,"text":"  "}]}`, wantErr: "empty translation at index 2"},
		{name: "broken json", reply: `{"translations": [`, wantErr: "no JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTranslations(tc.reply, 2)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
