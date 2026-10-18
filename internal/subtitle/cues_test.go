package subtitle

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitSentences(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"one sentence", "Just one sentence.", []string{"Just one sentence."}},
		{"two sentences", "He left. Then she came.", []string{"He left.", "Then she came."}},
		{"double space", "He left.  Then she came.", []string{"He left.", "Then she came."}},
		{"question and exclamation", "Really?! Yes! Go on.", []string{"Really?!", "Yes!", "Go on."}},
		{"closing quote", `He said "no." Then he left.`, []string{`He said "no."`, "Then he left."}},
		{"abbreviation", "Ask Dr. Smith about it.", []string{"Ask Dr. Smith about it."}},
		{"initial", "J. Smith arrived.", []string{"J. Smith arrived."}},
		{"decimal", "It costs 3.5 euros.", []string{"It costs 3.5 euros."}},
		{"lowercase continues", "Wait. and then more", []string{"Wait. and then more"}},
		{"turkish abbreviation", "Doç. Ahmet geldi. Sonra gitti.", []string{"Doç. Ahmet geldi.", "Sonra gitti."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, splitSentences(tc.text))
		})
	}
}

func TestSplitCue(t *testing.T) {
	t.Run("single sentence is unchanged", func(t *testing.T) {
		assert.Equal(t, []timedText{{1, 5, "Hello there."}}, splitCue(1, 5, "Hello there."))
	})
	t.Run("time is divided by length and pieces are contiguous", func(t *testing.T) {
		got := splitCue(0, 10, "Aaaaaaaaaaaaaaaaaaaa. Bbbbbbbbbbbbbbbbbbbb.")
		require.Len(t, got, 2)
		assert.Equal(t, 0.0, got[0].start)
		assert.Equal(t, 10.0, got[1].end)
		assert.Equal(t, got[0].end, got[1].start)
		assert.InDelta(t, 5, got[0].end, 0.5)
	})
	t.Run("short pieces are merged into their neighbour", func(t *testing.T) {
		assert.Equal(t, []timedText{{0, 3, "Yes. That is what we are going to do now."}}, splitCue(0, 3, "Yes. That is what we are going to do now."))
	})
}

func TestWrapText(t *testing.T) {
	assert.Equal(t, "Short line.", wrapText("Short line."))

	long := "This sentence is long enough that it needs two balanced lines."
	lines := strings.Split(wrapText(long), "\n")
	require.Len(t, lines, 2)
	for _, line := range lines {
		assert.LessOrEqual(t, utf8.RuneCountInString(line), lineLength, line)
	}
	// balanced: the lines differ by far less than the greedy 42/20 split would
	assert.InDelta(t, utf8.RuneCountInString(lines[0]), utf8.RuneCountInString(lines[1]), 12)
	assert.Equal(t, long, strings.Join(lines, " "), "no words are changed")
}

func TestSplitLong(t *testing.T) {
	text := "First part of the long cue, which goes on for a while, and then the second part continues for quite some time too."
	pieces := splitLong(0, 12, text)
	require.GreaterOrEqual(t, len(pieces), 2, "the cue is split")
	assert.Equal(t, 0.0, pieces[0].start)
	assert.Equal(t, 12.0, pieces[len(pieces)-1].end)

	var texts []string
	for index, piece := range pieces {
		assert.LessOrEqual(t, utf8.RuneCountInString(piece.text), lineLength*maxLines, piece.text)
		if index > 0 {
			assert.Equal(t, pieces[index-1].end, piece.start, "the pieces are contiguous")
		}
		texts = append(texts, piece.text)
	}
	assert.Equal(t, text, strings.Join(texts, " "))

	assert.Len(t, splitLong(0, 1, "short"), 1)
	assert.Len(t, splitLong(0, 1, strings.Repeat("x", 200)), 1, "text without spaces can't be split")
}

func TestTurnsFromWords(t *testing.T) {
	words := []sttWord{
		{Word: "Hello", Start: 0, End: 0.5, Speaker: "s0"},
		{Word: ",", Start: 0.5, End: 0.5, Speaker: "s0"},
		{Word: "there", Start: 0.6, End: 1, Speaker: "s0"},
		{Word: "(laughter)", Start: 1, End: 1.2, Speaker: "s1", Type: "audio_event"},
		{Word: "ghost", Start: 1.2, End: 1.3},
		{Word: "Hi", Start: 1.5, End: 1.8, Speaker: "s1"},
		{Word: "!", Start: 1.8, End: 1.8, Speaker: "s1"},
		{Word: " ", Start: 2, End: 2, Speaker: "s1"},
	}
	assert.Equal(t, []Segment{
		{Start: 0, End: 1, Text: "Hello, there", Speaker: "s0"},
		{Start: 1.5, End: 1.8, Text: "Hi!", Speaker: "s1"},
	}, turnsFromWords(words))
}

func TestGroupDialogue(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Segment
		want []timedText
	}{
		{
			name: "two speakers close together become one dialogue cue",
			in:   []Segment{{0, 1, "Hi", "a"}, {1.2, 2, "Hello", "b"}},
			want: []timedText{{0, 2, "- Hi\n- Hello"}},
		},
		{
			name: "same speaker stays separate",
			in:   []Segment{{0, 1, "Hi", "a"}, {1.2, 2, "Again", "a"}},
			want: []timedText{{0, 1, "Hi"}, {1.2, 2, "Again"}},
		},
		{
			name: "large gap stays separate",
			in:   []Segment{{0, 1, "Hi", "a"}, {5, 6, "Hello", "b"}},
			want: []timedText{{0, 1, "Hi"}, {5, 6, "Hello"}},
		},
		{
			name: "long line stays separate",
			in:   []Segment{{0, 1, "Hi", "a"}, {1.2, 2, strings.Repeat("x", 43), "b"}},
			want: []timedText{{0, 1, "Hi"}, {1.2, 2, strings.Repeat("x", 43)}},
		},
		{
			name: "no speaker labels",
			in:   []Segment{{0, 1, "Hi", ""}, {1.2, 2, "Hello", ""}},
			want: []timedText{{0, 1, "Hi"}, {1.2, 2, "Hello"}},
		},
		{
			name: "too long a span stays separate",
			in:   []Segment{{0, 3, "Hi", "a"}, {3.5, 8, "Hello", "b"}},
			want: []timedText{{0, 3, "Hi"}, {3.5, 8, "Hello"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, groupDialogue(tc.in))
		})
	}
}

func TestFormatCuesKeepsDialogueAndWrapsLongText(t *testing.T) {
	dialogue := timedText{0, 2, "- Hi\n- Hello"}
	long := timedText{2, 6, "This sentence is long enough that it needs two balanced lines."}
	got := formatCues([]timedText{dialogue, long})
	require.Len(t, got, 2)
	assert.Equal(t, dialogue, got[0])
	assert.Contains(t, got[1].text, "\n")
}

func TestBuildCuesKeepsCuesOrderedAndLongEnough(t *testing.T) {
	assert.Equal(t, []Cue{
		{1, "00:00:00,000 --> 00:00:02,000", "A"},
		{2, "00:00:02,000 --> 00:00:02,300", "B"},
		{3, "00:00:02,300 --> 00:00:03,000", "C"},
	}, buildCues([]timedText{{0, 2, "A"}, {1, 1.1, "B"}, {1.5, 3, "C"}}))
}
