package subtitle

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	dialogueLineLength  = 42
	maxDialogueGap      = 1.0
	maxDialogueDuration = 7.0
	lineLength          = 42
	maxLines            = 2
	minSplitDuration    = 1.0
	minCueDuration      = 0.3
)

// Segment is a timed piece of transcript in seconds. Speaker is a diarization label, empty if unknown.
type Segment struct {
	Start   float64
	End     float64
	Text    string
	Speaker string
}

// timedText is a cue before it is rendered: times in seconds and the text.
type timedText struct {
	start, end float64
	text       string
}

// lowercase tokens (en, tr) whose trailing period does not end a sentence
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "e.g": true, "i.e": true, "vs": true, "etc": true,
	"doç": true, "av": true, "sn": true, "vb": true, "bkz": true,
}

var (
	// End marks, optional closing quotes or brackets, then whitespace. Matches ".  " in "He left.  Then"
	// and '." ' in 'He said "no." Then'.
	sentenceEnd = regexp.MustCompile(`[.!?…]+["'”’)\]]*\s+`)
	lastWord    = regexp.MustCompile(`\S+$`)
	// whitespace in front of punctuation, left over from joining words with spaces ("Hi ,")
	detachedPunctuation = regexp.MustCompile(`\s+([,.!?;:…])`)
)

// turnsFromWords groups consecutive words with the same speaker into one Segment per speaker turn.
// Words without a speaker and audio events (laughter and the like) are skipped.
func turnsFromWords(words []sttWord) []Segment {
	var turns []Segment
	for _, word := range words {
		text := strings.TrimSpace(word.Word)
		if text == "" || word.Speaker == "" || word.Type == "audio_event" {
			continue
		}
		if last := len(turns) - 1; last >= 0 && turns[last].Speaker == string(word.Speaker) {
			turns[last].End = word.End
			turns[last].Text += " " + text
		} else {
			turns = append(turns, Segment{Start: word.Start, End: word.End, Text: text, Speaker: string(word.Speaker)})
		}
	}
	for position := range turns {
		turns[position].Text = detachedPunctuation.ReplaceAllString(turns[position].Text, "$1")
	}
	return turns
}

// groupDialogue turns segments into cues, pairing adjacent turns by different speakers into "- a" / "- b" cues.
// A pair is formed only if each line is short, the gap between them is small and the pair is brief.
func groupDialogue(segments []Segment) []timedText {
	var grouped []timedText
	for index := 0; index < len(segments); {
		cue := segments[index]
		if index+1 < len(segments) {
			following := segments[index+1]
			if cue.Speaker != "" && following.Speaker != "" && following.Speaker != cue.Speaker &&
				max(utf8.RuneCountInString(cue.Text), utf8.RuneCountInString(following.Text)) <= dialogueLineLength &&
				following.Start-cue.End <= maxDialogueGap &&
				following.End-cue.Start <= maxDialogueDuration {
				grouped = append(grouped, timedText{cue.Start, following.End, "- " + cue.Text + "\n- " + following.Text})
				index += 2
				continue
			}
		}
		grouped = append(grouped, timedText{cue.Start, cue.End, cue.Text})
		index++
	}
	return grouped
}

// splitSentences splits text at sentence ends. A boundary counts only if the next character starts a sentence
// (uppercase, digit or opening quote or bracket) and the word before a period is no known abbreviation or initial.
func splitSentences(text string) []string {
	var sentences []string
	start := 0
	for _, match := range sentenceEnd.FindAllStringIndex(text, -1) {
		following, _ := utf8.DecodeRuneInString(text[match[1]:])
		if following == utf8.RuneError || !(unicode.IsUpper(following) || unicode.IsDigit(following) || strings.ContainsRune("\"'“‘([", following)) {
			continue
		}
		matched := text[match[0]:match[1]]
		token := strings.TrimLeft(strings.ToLower(lastWord.FindString(text[start:match[0]])), "\"'“‘([")
		if strings.HasPrefix(matched, ".") && (abbreviations[token] || (utf8.RuneCountInString(token) == 1 && unicode.IsLetter([]rune(token)[0]))) {
			continue
		}
		sentences = append(sentences, strings.TrimSpace(text[start:match[0]+len(strings.TrimRightFunc(matched, unicode.IsSpace))]))
		start = match[1]
	}
	sentences = append(sentences, strings.TrimSpace(text[start:]))
	return slicesDeleteEmpty(sentences)
}

func slicesDeleteEmpty(values []string) []string {
	kept := values[:0]
	for _, value := range values {
		if value != "" {
			kept = append(kept, value)
		}
	}
	return kept
}

// splitCue splits a cue into one cue per sentence, dividing [start, end] in proportion to the sentence lengths.
// Pieces shorter than minSplitDuration are merged into their neighbour. A single sentence is returned unchanged.
func splitCue(start, end float64, text string) []timedText {
	sentences := splitSentences(text)
	if len(sentences) < 2 {
		return []timedText{{start, end, text}}
	}
	total := 0
	for _, sentence := range sentences {
		total += utf8.RuneCountInString(sentence)
	}
	pieces := make([]timedText, 0, len(sentences))
	cursor := start
	for _, sentence := range sentences {
		pieceEnd := cursor + (end-start)*float64(utf8.RuneCountInString(sentence))/float64(total)
		pieces = append(pieces, timedText{cursor, pieceEnd, sentence})
		cursor = pieceEnd
	}
	pieces[len(pieces)-1].end = end

	var merged []timedText
	for _, piece := range pieces {
		if last := len(merged) - 1; last >= 0 && (piece.end-piece.start < minSplitDuration || merged[last].end-merged[last].start < minSplitDuration) {
			merged[last].end = piece.end
			merged[last].text += " " + piece.text
		} else {
			merged = append(merged, piece)
		}
	}
	return merged
}

// wrapText wraps text into balanced lines of at most lineLength characters (60 characters become 2 lines of 30).
func wrapText(text string) string {
	length := utf8.RuneCountInString(text)
	if length <= lineLength {
		return text
	}
	linesNeeded := (length + lineLength - 1) / lineLength
	var lines []string
	// widen the target width until the words fit in the needed number of lines
	for width := (length + linesNeeded - 1) / linesNeeded; width <= lineLength; width++ {
		lines = wrapWords(text, width)
		if len(lines) <= linesNeeded {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// wrapWords greedily fills lines of at most width characters. A word longer than width gets a line of its own.
func wrapWords(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// splitLong splits a cue longer than maxLines lines into several cues, dividing [start, end] by length.
// It splits near the middle, preferring a spot right after a comma, semicolon, colon or sentence end
// (within the middle half of the text), else any space. It repeats until every piece fits.
func splitLong(start, end float64, text string) []timedText {
	characters := []rune(text)
	if len(characters) <= lineLength*maxLines {
		return []timedText{{start, end, text}}
	}
	middle := float64(len(characters)) / 2
	distance := func(position int) float64 {
		if gap := float64(position) - middle; gap < 0 {
			return -gap
		} else {
			return gap
		}
	}
	var spaces, clauseBreaks []int
	for position, character := range characters {
		if character != ' ' {
			continue
		}
		spaces = append(spaces, position)
		if position > 0 && strings.ContainsRune(",;:.!?", characters[position-1]) && distance(position) <= float64(len(characters))/4 {
			clauseBreaks = append(clauseBreaks, position)
		}
	}
	candidates := spaces
	if len(clauseBreaks) > 0 {
		candidates = clauseBreaks
	}
	if len(candidates) == 0 {
		return []timedText{{start, end, text}}
	}
	position := candidates[0]
	for _, candidate := range candidates[1:] {
		if distance(candidate) < distance(position) {
			position = candidate
		}
	}
	left, right := strings.TrimSpace(string(characters[:position])), strings.TrimSpace(string(characters[position:]))
	if left == "" || right == "" {
		return []timedText{{start, end, text}}
	}
	leftLength, rightLength := utf8.RuneCountInString(left), utf8.RuneCountInString(right)
	boundary := start + (end-start)*float64(leftLength)/float64(leftLength+rightLength)
	return append(splitLong(start, boundary, left), splitLong(boundary, end, right)...)
}

// formatCues splits single-speaker cues that are too long and wraps their text into lines.
// Dialogue cues pass through.
func formatCues(cues []timedText) []timedText {
	var formatted []timedText
	for _, cue := range cues {
		if strings.HasPrefix(cue.text, "- ") {
			formatted = append(formatted, cue)
			continue
		}
		for _, piece := range splitLong(cue.start, cue.end, cue.text) {
			formatted = append(formatted, timedText{piece.start, piece.end, wrapText(piece.text)})
		}
	}
	return formatted
}

// buildCues numbers the cues and keeps them from overlapping, with a minimum length of minCueDuration seconds.
func buildCues(cues []timedText) []Cue {
	built := make([]Cue, len(cues))
	previousEnd := 0.0
	for position, cue := range cues {
		start := max(cue.start, previousEnd)
		end := max(cue.end, start+minCueDuration)
		previousEnd = end
		built[position] = Cue{Index: position + 1, Timeline: Timeline(start, end), Text: cue.text}
	}
	return built
}
