package subtitle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/sourcegraph/conc/pool"

	"subbed/internal/logger"
)

const (
	contextCues           = 2
	maxValidationAttempts = 3
	// DefaultTranslateModel is used when no model is configured.
	DefaultTranslateModel = "google/gemini-3.5-flash-lite"
)

const translatePrompt = `You are a professional subtitle translator. Translate every item in "items" into %s.

Rules:
1. Return only one JSON object, no Markdown, no explanation: {"translations":[{"index":1,"text":"..."}]}
2. "translations" must contain exactly one entry per item, with the same "index" values. Never merge, split, skip or reorder items.
3. Translate each item at its own index even if word order differs; never move meaning to a neighbouring index.
4. Keep translations natural and concise, suitable for subtitles. Keep names, numbers, negation and tone accurate without removing profanity.
5. Use "context_before" and "context_after" only to resolve references and keep terminology consistent. Do not translate them.
6. Do not add notes. Keep line breaks only where the source has them.

Input:
%s`

// Translator translates SRT cues with an OpenRouter chat model, keeping them one to one.
type Translator struct {
	Client *Client
	Model  string
	// BatchSize is the number of cues per request
	BatchSize int
	// Workers is the number of parallel requests
	Workers int
	// Progress is called with the fraction of the batches that are done, once per finished batch. It is called
	// from several goroutines at a time and may be nil.
	Progress func(fraction float64)
}

// NewTranslator returns a Translator with batches of 20 cues and 8 parallel requests.
func NewTranslator(client *Client, model string) *Translator {
	return &Translator{Client: client, Model: model, BatchSize: 20, Workers: 8}
}

// TranslateResult is the result of a translation.
type TranslateResult struct {
	Cues []Cue
	// CostUSD is charged for all requests, repeated and failed batches included; 0 if the model doesn't
	// report it. It is set when Translate fails, too.
	CostUSD float64
}

// Translate translates the cues into language (a name the model understands, such as "Turkish"). The result has
// the same indices and time lines; only the text changes.
func (t *Translator) Translate(ctx context.Context, cues []Cue, language string) (TranslateResult, error) {
	if t.Client == nil || t.Model == "" {
		return TranslateResult{}, errors.New("translator needs a client and a model")
	}
	if t.BatchSize < 1 || t.Workers < 1 {
		return TranslateResult{}, errors.New("batch size and workers must be at least 1")
	}

	type bounds struct{ start, end int }
	var batches []bounds
	for start := 0; start < len(cues); start += t.BatchSize {
		batches = append(batches, bounds{start, min(start+t.BatchSize, len(cues))})
	}

	// the first failure cancels the batches still running and is the error that is returned
	workers := pool.New().WithContext(ctx).WithMaxGoroutines(t.Workers).WithCancelOnError().WithFirstError()
	translations := make([][]string, len(batches))
	batchCosts := make([]float64, len(batches))
	var finished atomic.Int32
	for position, batch := range batches {
		workers.Go(func(ctx context.Context) error {
			logger.FromCtx(ctx).Debug(fmt.Sprintf("Translating cues %d-%d of %d with %s", batch.start+1, batch.end, len(cues), t.Model))
			texts, costUSD, err := t.translateBatch(ctx, cues[batch.start:batch.end],
				cues[max(0, batch.start-contextCues):batch.start], cues[batch.end:min(batch.end+contextCues, len(cues))], language)
			translations[position], batchCosts[position] = texts, costUSD
			if err == nil && t.Progress != nil {
				t.Progress(float64(finished.Add(1)) / float64(len(batches)))
			}
			return err
		})
	}
	err := workers.Wait()
	costUSD := 0.0
	for _, batchCost := range batchCosts {
		costUSD += batchCost
	}
	if err != nil {
		return TranslateResult{CostUSD: costUSD}, err
	}

	translated := make([]Cue, 0, len(cues))
	for _, texts := range translations {
		for _, text := range texts {
			cue := cues[len(translated)]
			cue.Text = text
			translated = append(translated, cue)
		}
	}
	return TranslateResult{Cues: translated, CostUSD: costUSD}, nil
}

// translateBatch translates one batch, with the before and after cues sent only as context. An invalid reply
// (wrong count, bad indices, empty text) is retried up to maxValidationAttempts times with the problem appended
// to the prompt. It returns one text per cue, in order, and the USD charged for all attempts.
func (t *Translator) translateBatch(ctx context.Context, cues, before, after []Cue, language string) ([]string, float64, error) {
	type item struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	}
	input := struct {
		Before []string `json:"context_before"`
		Items  []item   `json:"items"`
		After  []string `json:"context_after"`
	}{Before: texts(before), Items: make([]item, len(cues)), After: texts(after)}
	for position, cue := range cues {
		input.Items[position] = item{Index: position + 1, Text: cue.Text}
	}
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(input); err != nil {
		return nil, 0, err
	}
	prompt := fmt.Sprintf(translatePrompt, language, strings.TrimSpace(payload.String()))

	lastProblem := ""
	costUSD := 0.0
	for attempt := 1; attempt <= maxValidationAttempts; attempt++ {
		content := prompt
		if lastProblem != "" {
			content += fmt.Sprintf("\n\nYour previous answer was invalid: %s. Return the complete corrected JSON.", lastProblem)
		}
		response, err := t.Client.Post(ctx, "/chat/completions", map[string]any{
			"model":           t.Model,
			"temperature":     0.2,
			"response_format": map[string]string{"type": "json_object"},
			"messages":        []map[string]string{{"role": "user", "content": content}},
		})
		if err != nil {
			return nil, costUSD, err
		}
		costUSD += response.CostUSD
		if response.Status != 200 {
			return nil, costUSD, fmt.Errorf("translation failed with status %d: %.300s", response.Status, response.Body)
		}

		reply, err := replyContent(response.Body)
		if err == nil {
			var translated []string
			if translated, err = parseTranslations(reply, len(cues)); err == nil {
				return translated, costUSD, nil
			}
		}
		lastProblem = err.Error()
		logger.FromCtx(ctx).Warn(fmt.Sprintf("Invalid response (%s), attempt %d/%d", lastProblem, attempt, maxValidationAttempts))
	}
	return nil, costUSD, fmt.Errorf("batch starting at cue %d failed: %s", cues[0].Index, lastProblem)
}

func texts(cues []Cue) []string {
	result := make([]string, len(cues))
	for position, cue := range cues {
		result[position] = cue.Text
	}
	return result
}

// replyContent returns the text of the first choice of a chat completion.
func replyContent(body []byte) (string, error) {
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return "", fmt.Errorf("unreadable response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", errors.New("response has no choices")
	}
	return completion.Choices[0].Message.Content, nil
}

// parseTranslations extracts the {"translations": [{"index", "text"}]} object from a model reply. It takes
// everything from the first "{" to the last "}", so Markdown fences around the JSON are ignored. The texts are
// returned ordered by index; there must be exactly one non-empty entry per index.
func parseTranslations(reply string, expected int) ([]string, error) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return nil, errors.New("no JSON object in response")
	}
	var parsed struct {
		Translations []struct {
			Index *int   `json:"index"`
			Text  string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(parsed.Translations) != expected {
		return nil, fmt.Errorf("expected %d translations, got %d", expected, len(parsed.Translations))
	}
	result := make([]string, expected)
	for _, entry := range parsed.Translations {
		if entry.Index == nil {
			return nil, errors.New("entry without an index")
		}
		if *entry.Index < 1 || *entry.Index > expected || result[*entry.Index-1] != "" {
			return nil, fmt.Errorf("invalid or duplicate index %d", *entry.Index)
		}
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			return nil, fmt.Errorf("empty translation at index %d", *entry.Index)
		}
		result[*entry.Index-1] = text
	}
	return result, nil
}
