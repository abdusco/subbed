package subtitle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSRT(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []Cue
		wantErr bool
	}{
		{
			name:    "two cues with CRLF and a BOM",
			content: "\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,500\r\nWorld\r\nagain\r\n",
			want: []Cue{
				{Index: 1, Timeline: "00:00:01,000 --> 00:00:02,000", Text: "Hello"},
				{Index: 2, Timeline: "00:00:03,000 --> 00:00:04,500", Text: "World\nagain"},
			},
		},
		{
			name:    "missing index lines and odd numbering are renumbered",
			content: "00:00:01.000 --> 00:00:02.000\nA\n\n7\n00:00:03,000 --> 00:00:04,000\nB\n",
			want: []Cue{
				{Index: 1, Timeline: "00:00:01.000 --> 00:00:02.000", Text: "A"},
				{Index: 2, Timeline: "00:00:03,000 --> 00:00:04,000", Text: "B"},
			},
		},
		{
			name:    "blank line with spaces separates cues",
			content: "1\n00:00:01,000 --> 00:00:02,000\nA\n  \n2\n00:00:03,000 --> 00:00:04,000\nB",
			want: []Cue{
				{Index: 1, Timeline: "00:00:01,000 --> 00:00:02,000", Text: "A"},
				{Index: 2, Timeline: "00:00:03,000 --> 00:00:04,000", Text: "B"},
			},
		},
		{name: "empty", content: " \n ", wantErr: true},
		{name: "block without a time line", content: "1\nhello\nworld\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSRT(tc.content)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRenderSRTRoundTrips(t *testing.T) {
	cues := []Cue{
		{Index: 1, Timeline: "00:00:01,000 --> 00:00:02,000", Text: "Hello"},
		{Index: 2, Timeline: "00:00:03,000 --> 00:00:04,500", Text: "- Hi\n- Hey"},
	}
	rendered := RenderSRT(cues)
	assert.Equal(t, "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,500\n- Hi\n- Hey\n", rendered)

	parsed, err := ParseSRT(rendered)
	require.NoError(t, err)
	assert.Equal(t, cues, parsed)
}

func TestFormatTimestamp(t *testing.T) {
	for seconds, want := range map[float64]string{
		0:        "00:00:00,000",
		1.5:      "00:00:01,500",
		59.9996:  "00:01:00,000",
		3661.007: "01:01:01,007",
		-3:       "00:00:00,000",
	} {
		assert.Equal(t, want, FormatTimestamp(seconds), "%v seconds", seconds)
	}
	assert.Equal(t, "00:00:01,000 --> 00:00:02,250", Timeline(1, 2.25))
}
