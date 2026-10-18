package subtitle

import (
	"reflect"
	"testing"
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
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cues = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRenderSRTRoundTrips(t *testing.T) {
	cues := []Cue{
		{Index: 1, Timeline: "00:00:01,000 --> 00:00:02,000", Text: "Hello"},
		{Index: 2, Timeline: "00:00:03,000 --> 00:00:04,500", Text: "- Hi\n- Hey"},
	}
	rendered := RenderSRT(cues)
	want := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,500\n- Hi\n- Hey\n"
	if rendered != want {
		t.Fatalf("rendered = %q, want %q", rendered, want)
	}
	parsed, err := ParseSRT(rendered)
	if err != nil || !reflect.DeepEqual(parsed, cues) {
		t.Fatalf("round trip = %+v, %v", parsed, err)
	}
}

func TestFormatTimestamp(t *testing.T) {
	for seconds, want := range map[float64]string{
		0:        "00:00:00,000",
		1.5:      "00:00:01,500",
		59.9996:  "00:01:00,000",
		3661.007: "01:01:01,007",
		-3:       "00:00:00,000",
	} {
		if got := FormatTimestamp(seconds); got != want {
			t.Errorf("FormatTimestamp(%v) = %q, want %q", seconds, got, want)
		}
	}
	if got := Timeline(1, 2.25); got != "00:00:01,000 --> 00:00:02,250" {
		t.Errorf("Timeline = %q", got)
	}
}
