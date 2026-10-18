// Package subtitle turns audio into SRT subtitles and translates them, using OpenRouter models.
package subtitle

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Cue is one SRT cue: its 1-based index, the time line as written ("00:00:01,000 --> 00:00:02,500") and its text.
type Cue struct {
	Index    int
	Timeline string
	Text     string
}

var (
	// blank line (possibly containing spaces) between SRT blocks
	blockSeparator = regexp.MustCompile(`\n\s*\n`)
	// start of an SRT time line; the milliseconds may be separated by a comma or a dot
	timelinePattern = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}[,.]\d{3}\s+-->\s+\d{2}:\d{2}:\d{2}[,.]\d{3}`)
)

// ParseSRT parses SRT text into cues, tolerating CRLF line endings, a BOM and a missing index line.
// Cues are renumbered from 1 and their time lines are kept as written.
func ParseSRT(content string) ([]Cue, error) {
	content = strings.TrimPrefix(content, "\xef\xbb\xbf")
	content = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(content)
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("SRT contains no cues")
	}

	var cues []Cue
	for _, block := range blockSeparator.Split(content, -1) {
		lines := strings.Split(block, "\n")
		timeline := -1
		for position, line := range lines[:min(3, len(lines))] {
			if timelinePattern.MatchString(strings.TrimSpace(line)) {
				timeline = position
				break
			}
		}
		if timeline < 0 {
			return nil, fmt.Errorf("invalid SRT block: %.80q", block)
		}
		cues = append(cues, Cue{
			Index:    len(cues) + 1,
			Timeline: strings.TrimSpace(lines[timeline]),
			Text:     strings.TrimSpace(strings.Join(lines[timeline+1:], "\n")),
		})
	}
	return cues, nil
}

// RenderSRT renders cues as SRT text.
func RenderSRT(cues []Cue) string {
	blocks := make([]string, len(cues))
	for position, cue := range cues {
		blocks[position] = fmt.Sprintf("%d\n%s\n%s\n", cue.Index, cue.Timeline, cue.Text)
	}
	return strings.Join(blocks, "\n")
}

// FormatTimestamp formats seconds as an SRT timestamp, HH:MM:SS,mmm.
func FormatTimestamp(seconds float64) string {
	total := int64(math.Round(max(seconds, 0) * 1000))
	hours, total := total/3_600_000, total%3_600_000
	minutes, total := total/60_000, total%60_000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, total/1000, total%1000)
}

// Timeline formats a start and end time in seconds as an SRT time line.
func Timeline(start, end float64) string {
	return FormatTimestamp(start) + " --> " + FormatTimestamp(end)
}
