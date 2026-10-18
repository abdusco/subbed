package subtitle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 {
		t.Errorf("bad request: %v", err)
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
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		t.Errorf("prompt input is not JSON: %v\n%s", err, input)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d cues", len(got))
	}
	for position, cue := range got {
		if cue.Index != cues[position].Index || cue.Timeline != cues[position].Timeline || cue.Text != "tr:"+cues[position].Text {
			t.Errorf("cue %d = %+v", position, cue)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("%d requests, want 3", len(seen))
	}
	middle := seen["line 3"]
	if !reflect.DeepEqual(middle.Before, []string{"line 1", "line 2"}) || !reflect.DeepEqual(middle.After, []string{"line 5"}) || len(middle.Items) != 2 {
		t.Errorf("middle batch = %+v", middle)
	}
	if first := seen["line 1"]; len(first.Before) != 0 || !strings.Contains(first.Prompt, "into Turkish.") {
		t.Errorf("first batch = %+v", first)
	}
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
	if err != nil || got[0].Text != "ok:line 1" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(prompts) != 2 || strings.Contains(prompts[0], "previous answer") || !strings.Contains(prompts[1], "Your previous answer was invalid: expected 2 translations, got 1") {
		t.Errorf("prompts = %q", prompts)
	}
}

func TestTranslateGivesUpAfterRepeatedInvalidReplies(t *testing.T) {
	var calls int
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		calls++
		w.Write([]byte(completion("I cannot do that")))
	})

	_, err := translator.Translate(context.Background(), testCues(3), "German")
	if err == nil || !strings.Contains(err.Error(), "batch starting at cue 1") || calls != maxValidationAttempts {
		t.Fatalf("error = %v after %d calls", err, calls)
	}
}

func TestTranslateStopsOnClientErrors(t *testing.T) {
	var calls int
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		calls++
		http.Error(w, "bad key", http.StatusUnauthorized)
	})

	_, err := translator.Translate(context.Background(), testCues(2), "German")
	if err == nil || !strings.Contains(err.Error(), "401") || calls != 1 {
		t.Fatalf("error = %v after %d calls", err, calls)
	}
}

func TestTranslateKeepsCharactersUnescaped(t *testing.T) {
	var prompt string
	translator := newTestTranslator(t, func(request chatRequest, call int, w http.ResponseWriter) {
		prompt = request.Prompt
		w.Write([]byte(translationsFor(request, "")))
	})
	cues := []Cue{{Index: 1, Timeline: "00:00:01,000 --> 00:00:02,000", Text: "Tom & Jerry <3 çay"}}
	if _, err := translator.Translate(context.Background(), cues, "Turkish"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Tom & Jerry <3 çay") {
		t.Errorf("prompt escaped the text: %s", prompt)
	}
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
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}
