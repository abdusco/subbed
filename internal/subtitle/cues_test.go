package subtitle

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
			if got := splitSentences(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitSentences(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestSplitCue(t *testing.T) {
	t.Run("single sentence is unchanged", func(t *testing.T) {
		got := splitCue(1, 5, "Hello there.")
		if !reflect.DeepEqual(got, []timedText{{1, 5, "Hello there."}}) {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("time is divided by length and pieces are contiguous", func(t *testing.T) {
		got := splitCue(0, 10, "Aaaaaaaaaaaaaaaaaaaa. Bbbbbbbbbbbbbbbbbbbb.")
		if len(got) != 2 || got[0].start != 0 || got[1].end != 10 || got[0].end != got[1].start {
			t.Fatalf("got %+v", got)
		}
		if got[0].end < 4.5 || got[0].end > 5.5 {
			t.Errorf("boundary = %v, want about 5", got[0].end)
		}
	})
	t.Run("short pieces are merged into their neighbour", func(t *testing.T) {
		got := splitCue(0, 3, "Yes. That is what we are going to do now.")
		if len(got) != 1 || got[0].text != "Yes. That is what we are going to do now." || got[0].end != 3 {
			t.Errorf("got %+v", got)
		}
	})
}

func TestWrapText(t *testing.T) {
	short := "Short line."
	if got := wrapText(short); got != short {
		t.Errorf("short text changed: %q", got)
	}
	long := "This sentence is long enough that it needs two balanced lines."
	got := wrapText(long)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) > lineLength {
			t.Errorf("line too long: %q", line)
		}
	}
	// balanced: the lines differ by far less than the greedy 42/20 split would
	if diff := utf8.RuneCountInString(lines[0]) - utf8.RuneCountInString(lines[1]); diff > 12 || diff < -12 {
		t.Errorf("unbalanced lines: %q", lines)
	}
	if strings.Join(lines, " ") != long {
		t.Errorf("words changed: %q", got)
	}
}

func TestSplitLong(t *testing.T) {
	text := "First part of the long cue, which goes on for a while, and then the second part continues for quite some time too."
	pieces := splitLong(0, 12, text)
	if len(pieces) < 2 {
		t.Fatalf("not split: %+v", pieces)
	}
	if pieces[0].start != 0 || pieces[len(pieces)-1].end != 12 {
		t.Errorf("span changed: %+v", pieces)
	}
	var texts []string
	for index, piece := range pieces {
		if utf8.RuneCountInString(piece.text) > lineLength*maxLines {
			t.Errorf("piece too long: %q", piece.text)
		}
		if index > 0 && piece.start != pieces[index-1].end {
			t.Errorf("gap before %+v", piece)
		}
		texts = append(texts, piece.text)
	}
	if strings.Join(texts, " ") != text {
		t.Errorf("text changed: %q", texts)
	}
	if got := splitLong(0, 1, "short"); len(got) != 1 {
		t.Errorf("short cue split: %+v", got)
	}
	if got := splitLong(0, 1, strings.Repeat("x", 200)); len(got) != 1 {
		t.Errorf("text without spaces split: %+v", got)
	}
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
	want := []Segment{
		{Start: 0, End: 1, Text: "Hello, there", Speaker: "s0"},
		{Start: 1.5, End: 1.8, Text: "Hi!", Speaker: "s1"},
	}
	if got := turnsFromWords(words); !reflect.DeepEqual(got, want) {
		t.Errorf("turns = %+v, want %+v", got, want)
	}
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
			if got := groupDialogue(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFormatCuesKeepsDialogueAndWrapsLongText(t *testing.T) {
	dialogue := timedText{0, 2, "- Hi\n- Hello"}
	long := timedText{2, 6, "This sentence is long enough that it needs two balanced lines."}
	got := formatCues([]timedText{dialogue, long})
	if len(got) != 2 || got[0] != dialogue || !strings.Contains(got[1].text, "\n") {
		t.Errorf("got %+v", got)
	}
}

func TestBuildCuesKeepsCuesOrderedAndLongEnough(t *testing.T) {
	got := buildCues([]timedText{{0, 2, "A"}, {1, 1.1, "B"}, {1.5, 3, "C"}})
	want := []Cue{
		{1, "00:00:00,000 --> 00:00:02,000", "A"},
		{2, "00:00:02,000 --> 00:00:02,300", "B"},
		{3, "00:00:02,300 --> 00:00:03,000", "C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
