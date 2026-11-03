package astisub

import (
	"bytes"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SCC command words with odd parity
var (
	sccWRC = sccWord(0x14, sccCtrlRCL)
	sccWEN = sccWord(0x14, sccCtrlENM)
	sccWEO = sccWord(0x14, sccCtrlEOC)
	sccWED = sccWord(0x14, sccCtrlEDM)
	sccWCR = sccWord(0x14, sccCtrlCR)
	sccWR3 = sccWord(0x14, sccCtrlRU3)
	sccWDC = sccWord(0x14, sccCtrlRDC)
)

// sccRow builds a timecode row
func sccRow(tc string, words ...string) string {
	return tc + "\t" + strings.Join(words, " ")
}

// sccContent builds a complete SCC document
func sccContent(rows ...string) string {
	return sccHeader + "\r\n\r\n" + strings.Join(rows, "\r\n") + "\r\n"
}

// sccTextWords encodes ASCII text into parity protected code words
func sccTextWords(s string) []string {
	bs := []byte(s)
	var out []string
	for len(bs) >= 2 {
		out = append(out, sccWord(bs[0], bs[1]))
		bs = bs[2:]
	}
	if len(bs) == 1 {
		out = append(out, sccWord(bs[0], 0x00))
	}
	return out
}

func sccItemLineText(item *Item, idx int) string {
	var s string
	for _, li := range item.Lines[idx].Items {
		s += li.Text
	}
	return s
}

func TestParseSCCTimecode(t *testing.T) {
	// Non drop frame, 30 frames per second
	for _, c := range []struct {
		tc   string
		want time.Duration
	}{
		{"00:00:00:01", time.Second / 30},
		{"00:00:00:15", 15 * time.Second / 30},
		{"00:00:01:00", time.Second},
		{"00:01:00:00", time.Minute},
		{"01:00:00:00", time.Hour},
	} {
		d, err := parseSCCTimecode(c.tc)
		require.NoError(t, err, c.tc)
		assert.Equal(t, c.want, d, c.tc)
	}

	// Drop frame: 30000/1001 frames per second, one frame tolerance
	frame := time.Duration(1001 * int64(time.Second) / 30000)
	for _, c := range []struct {
		tc     string
		frames int64
	}{
		{"00:01:00;00", 1798},  // 1800 - 2 dropped frames
		{"00:09:00;00", 16182}, // 16200 - 18
		{"00:10:00;00", 17982}, // every 10th minute drops nothing extra: 18000 - 18
		{"01:00:00;00", 107892},
	} {
		d, err := parseSCCTimecode(c.tc)
		require.NoError(t, err, c.tc)
		want := time.Duration(float64(c.frames) * 1001.0 / 30000.0 * float64(time.Second))
		assert.InDelta(t, int64(want), int64(d), float64(frame+time.Millisecond), c.tc)
	}

	// Invalid
	for _, tc := range []string{
		"",
		"0:00:00:00",
		"00:00:00",
		"00:00:00#00",
		"00:00:00:30",
		"00:60:00:00",
		"00:00:60:00",
		"xx:00:00:00",
	} {
		_, err := parseSCCTimecode(tc)
		assert.Error(t, err, tc)
	}
}

func TestReadFromSCCErrors(t *testing.T) {
	for _, c := range []struct {
		desc    string
		content string
	}{
		{"empty", ""},
		{"bad header", "Scenarist_SCC V2.0\r\n"},
		{"missing tab", sccHeader + "\r\n00:00:00:00 9420\r\n"},
		{"invalid timecode", sccHeader + "\r\n00:00:00:30\t9420\r\n"},
		{"short code word", sccHeader + "\r\n00:00:00:00\t942\r\n"},
		{"long code word", sccHeader + "\r\n00:00:00:00\t94200\r\n"},
		{"non hex word", sccHeader + "\r\n00:00:00:00\t942g\r\n"},
		{"bad parity first byte", sccHeader + "\r\n00:00:00:00\t1420\r\n"},
		{"bad parity second byte", sccHeader + "\r\n00:00:00:00\t9421\r\n"},
	} {
		_, err := ReadFromSCC(strings.NewReader(c.content))
		assert.Error(t, err, c.desc)
	}
}

func TestReadFromSCCBOM(t *testing.T) {
	content := "\ufeff" + sccContent(
		sccRow("00:00:01:00", sccWRC, sccWRC, sccWEN, sccWEN,
			sccWord(0x14, 0x60), sccWord(0x14, 0x60)),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	// No text: no caption output, but framerate is still set
	assert.Empty(t, s.Items)
	require.NotNil(t, s.Metadata)
	assert.Equal(t, 30, s.Metadata.Framerate)
}

func TestReadFromSCCPopOn(t *testing.T) {
	load := append([]string{sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(0x14, 0x60), sccWord(0x14, 0x60)}, sccTextWords("HELLO")...) // PAC row 15 (94e0)
	content := sccContent(
		sccRow("00:00:01:00", load...),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	item := s.Items[0]
	assert.Equal(t, 2*time.Second, item.StartAt) // caption starts at EOC
	assert.Equal(t, 3*time.Second, item.EndAt)
	require.Len(t, item.Lines, 1)
	assert.Equal(t, "HELLO", sccItemLineText(item, 0))
	assert.Nil(t, item.Lines[0].Items[0].InlineStyle)

	// Two rows: ordered top to bottom
	two := []string{sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(0x14, 0x40), sccWord(0x14, 0x40)} // PAC row 14
	two = append(two, sccTextWords("TOP")...)
	two = append(two, sccWord(0x14, 0x60), sccWord(0x14, 0x60)) // PAC row 15
	two = append(two, sccTextWords("BOTTOM")...)
	content = sccContent(
		sccRow("00:00:01:00", two...),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err = ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	require.Len(t, s.Items[0].Lines, 2)
	assert.Equal(t, "TOP", sccItemLineText(s.Items[0], 0))
	assert.Equal(t, "BOTTOM", sccItemLineText(s.Items[0], 1))
}

func TestReadFromSCCPACRows(t *testing.T) {
	// Anchors: row 1 = 9140, row 5 = 1540, row 11 = 9040, row 15 = 94e0
	w := []string{sccWRC, sccWRC, sccWEN, sccWEN}
	w = append(w, sccWord(0x11, 0x40), sccWord(0x11, 0x40))
	w = append(w, sccTextWords("R1")...)
	w = append(w, sccWord(0x15, 0x40), sccWord(0x15, 0x40))
	w = append(w, sccTextWords("R5")...)
	w = append(w, sccWord(0x10, 0x40), sccWord(0x10, 0x40))
	w = append(w, sccTextWords("R11")...)
	w = append(w, sccWord(0x14, 0x60), sccWord(0x14, 0x60))
	w = append(w, sccTextWords("R15")...)
	content := sccContent(
		sccRow("00:00:01:00", w...),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	lines := s.Items[0].Lines
	require.Len(t, lines, 4)
	assert.Equal(t, "R1", sccItemLineText(s.Items[0], 0))
	assert.Equal(t, "R5", sccItemLineText(s.Items[0], 1))
	assert.Equal(t, "R11", sccItemLineText(s.Items[0], 2))
	assert.Equal(t, "R15", sccItemLineText(s.Items[0], 3))
}

// sccStyledItem builds a pop on caption with a single PAC style and text
func sccStyledItem(pacA, pacB byte, text string) []string {
	w := []string{
		sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(pacA, pacB), sccWord(pacA, pacB),
	}
	w = append(w, sccTextWords(text)...)
	return []string{
		sccRow("00:00:01:00", w...),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	}
}

func TestReadFromSCCPACStyles(t *testing.T) {
	// Colors: 00-0d pairs, odd values underlined
	for low := byte(0x00); low <= 0x0d; low++ {
		rows := sccStyledItem(0x11, 0x40+low, "X")
		s, err := ReadFromSCC(strings.NewReader(sccContent(rows...)))
		require.NoError(t, err)
		style := s.Items[0].Lines[0].Items[0].InlineStyle
		underline := low%2 == 1
		color := sccPACColors[low/2]
		if color == nil && !underline {
			assert.Nil(t, style, "low %#x", low)
		} else {
			require.NotNil(t, style, "low %#x", low)
			assert.Equal(t, underline, style.WebVTTUnderline, "low %#x", low)
			if color == nil {
				assert.Nil(t, style.TTMLColor, "low %#x", low)
			} else {
				require.NotNil(t, style.TTMLColor, "low %#x", low)
				assert.Equal(t, color.Red, style.TTMLColor.Red, "low %#x", low)
				assert.Equal(t, color.Green, style.TTMLColor.Green, "low %#x", low)
				assert.Equal(t, color.Blue, style.TTMLColor.Blue, "low %#x", low)
			}
		}
	}

	// White italics
	s, err := ReadFromSCC(strings.NewReader(sccContent(sccStyledItem(0x11, 0x4e, "X")...)))
	require.NoError(t, err)
	style := s.Items[0].Lines[0].Items[0].InlineStyle
	require.NotNil(t, style)
	assert.True(t, style.WebVTTItalics)
	assert.False(t, style.WebVTTUnderline)

	// White italics underlined
	s, err = ReadFromSCC(strings.NewReader(sccContent(sccStyledItem(0x11, 0x4f, "X")...)))
	require.NoError(t, err)
	style = s.Items[0].Lines[0].Items[0].InlineStyle
	require.NotNil(t, style)
	assert.True(t, style.WebVTTItalics)
	assert.True(t, style.WebVTTUnderline)

	// Indentations: 10-1f pairs of indent 0,4,...,28 columns
	for low := byte(0x10); low <= 0x1f; low++ {
		rows := sccStyledItem(0x11, 0x40+low, "HI")
		s, err = ReadFromSCC(strings.NewReader(sccContent(rows...)))
		require.NoError(t, err)
		text := sccItemLineText(s.Items[0], 0)
		indent := int((low-0x10)/2) * 4
		assert.Equal(t, strings.Repeat(" ", indent)+"HI", text, "low %#x", low)
		style = s.Items[0].Lines[0].Items[0].InlineStyle
		if low%2 == 1 {
			require.NotNil(t, style, "low %#x", low)
			assert.True(t, style.WebVTTUnderline, "low %#x", low)
		} else {
			assert.True(t, style == nil || !style.WebVTTUnderline, "low %#x", low)
		}
	}
}

func TestReadFromSCCCharacters(t *testing.T) {
	// Basic CEA-608 differences
	w := []string{sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(0x14, 0x60), sccWord(0x14, 0x60)}
	w = append(w,
		sccWord(0x2a, 0x5c), // á é
		sccWord(0x5e, 0x5f), // í ó
		sccWord(0x60, 0x7b), // ú ç
		sccWord(0x7c, 0x7d), // ÷ Ñ
		sccWord(0x7e, 0x7f), // ñ █
		sccWord(0x11, 0x30), // ®
		sccWord(0x11, 0x37), // ♪
		sccWord(0x11, 0x39), // transparent space -> regular space
		sccWord(0x11, 0x3f), // û
		sccWord(0x11, 0x37), // ♪
		sccWord(0x11, 0x37), // ♪ twice in a row are two characters
	)
	content := sccContent(
		sccRow("00:00:01:00", w...),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "áéíóúç÷Ññ█®♪ û♪♪", sccItemLineText(s.Items[0], 0))

	// Extended characters delete the previously written character first:
	// "CA" followed by 12 20 (Á) yields "CÁ"; 13 20 is Ã.
	w = []string{sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(0x14, 0x60), sccWord(0x14, 0x60)}
	w = append(w, sccTextWords("CA")...)
	w = append(w, sccWord(0x12, 0x20)) // Á replaces A
	w = append(w, sccTextWords(" / ")...)
	w = append(w, sccTextWords("a")...)
	w = append(w, sccWord(0x13, 0x20)) // Ã replaces a
	content = sccContent(
		sccRow("00:00:01:00", w...),
		sccRow("00:00:02:00", sccWEO, sccWEO),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err = ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, "CÁ / Ã", sccItemLineText(s.Items[0], 0))
}

func TestReadFromSCCRollUp(t *testing.T) {
	content := sccContent(
		sccRow("00:00:00:00", append([]string{sccWR3, sccWR3,
			sccWord(0x14, 0x60), sccWord(0x14, 0x60)}, sccTextWords("A")...)...),
		sccRow("00:00:01:00", append([]string{sccWCR, sccWCR,
			sccWord(0x14, 0x60), sccWord(0x14, 0x60)}, sccTextWords("B")...)...),
		sccRow("00:00:02:00", append([]string{sccWCR, sccWCR,
			sccWord(0x14, 0x60), sccWord(0x14, 0x60)}, sccTextWords("C")...)...),
		sccRow("00:00:03:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 3)

	assert.Equal(t, 0, int(s.Items[0].StartAt))
	assert.Equal(t, int(time.Second), int(s.Items[0].EndAt))
	require.Len(t, s.Items[0].Lines, 1)
	assert.Equal(t, "A", sccItemLineText(s.Items[0], 0))

	assert.Equal(t, int(time.Second), int(s.Items[1].StartAt))
	assert.Equal(t, 2*int(time.Second), int(s.Items[1].EndAt))
	require.Len(t, s.Items[1].Lines, 2)
	assert.Equal(t, "A", sccItemLineText(s.Items[1], 0))
	assert.Equal(t, "B", sccItemLineText(s.Items[1], 1))

	assert.Equal(t, 2*int(time.Second), int(s.Items[2].StartAt))
	assert.Equal(t, 3*int(time.Second), int(s.Items[2].EndAt))
	require.Len(t, s.Items[2].Lines, 3)
	assert.Equal(t, "A", sccItemLineText(s.Items[2], 0))
	assert.Equal(t, "B", sccItemLineText(s.Items[2], 1))
	assert.Equal(t, "C", sccItemLineText(s.Items[2], 2))
}

func TestReadFromSCCPaintOn(t *testing.T) {
	content := sccContent(
		sccRow("00:00:00:00", append([]string{sccWDC, sccWDC,
			sccWord(0x14, 0x60), sccWord(0x14, 0x60)}, sccTextWords("HI")...)...),
		sccRow("00:00:01:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, time.Duration(0), s.Items[0].StartAt)
	assert.Equal(t, time.Second, s.Items[0].EndAt)
	require.Len(t, s.Items[0].Lines, 1)
	assert.Equal(t, "HI", sccItemLineText(s.Items[0], 0))
}

func TestReadFromSCCEmptyAndTrailingSpaces(t *testing.T) {
	// No text at all: nothing is output
	content := sccContent(
		sccRow("00:00:00:00", sccWRC, sccWRC, sccWEN, sccWEN),
		sccRow("00:00:01:00", sccWEO, sccWEO),
		sccRow("00:00:02:00", sccWED, sccWED),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	assert.Empty(t, s.Items)

	// Trailing spaces are removed
	w := []string{sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(0x14, 0x60), sccWord(0x14, 0x60)}
	w = append(w, sccTextWords("HI   ")...)
	content = sccContent(
		sccRow("00:00:00:00", w...),
		sccRow("00:00:01:00", sccWEO, sccWEO),
		sccRow("00:00:02:00", sccWED, sccWED),
	)
	s, err = ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "HI", sccItemLineText(s.Items[0], 0))
}

func TestReadFromSCCUnclosedPopOn(t *testing.T) {
	// EOF without EDM closes the caption at the last timecode
	w := []string{sccWRC, sccWRC, sccWEN, sccWEN,
		sccWord(0x14, 0x60), sccWord(0x14, 0x60)}
	w = append(w, sccTextWords("HI")...)
	content := sccContent(
		sccRow("00:00:00:00", w...),
		sccRow("00:00:01:00", sccWEO, sccWEO),
	)
	s, err := ReadFromSCC(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, time.Second, s.Items[0].StartAt)
	assert.Equal(t, time.Second, s.Items[0].EndAt)
}

func TestWriteToSCCErrors(t *testing.T) {
	assert.EqualError(t, Subtitles{}.WriteToSCC(&bytes.Buffer{}), ErrNoSubtitlesToWrite.Error())

	// Items without any text also produce no output
	s := NewSubtitles()
	s.Items = append(s.Items, &Item{
		StartAt: time.Second,
		EndAt:   2 * time.Second,
		Lines:   []Line{{Items: []LineItem{{Text: "   "}}}},
	})
	assert.EqualError(t, s.WriteToSCC(&bytes.Buffer{}), ErrNoSubtitlesToWrite.Error())
}

func sccCheckOddParity(t *testing.T, content string) {
	for _, line := range strings.Split(content, "\r\n") {
		idx := strings.IndexByte(line, '\t')
		if idx < 0 {
			continue
		}
		for _, tok := range strings.Fields(line[idx+1:]) {
			v, err := strconv.ParseUint(tok, 16, 16)
			require.NoError(t, err)
			assert.Equal(t, 1, bits.OnesCount8(byte(v>>8))%2, tok)
			assert.Equal(t, 1, bits.OnesCount8(byte(v))%2, tok)
		}
	}
}

func TestWriteToSCCAndRoundTrip(t *testing.T) {
	s := NewSubtitles()
	s.Items = append(s.Items,
		&Item{
			StartAt: time.Second,
			EndAt:   2 * time.Second,
			Lines: []Line{
				{Items: []LineItem{{Text: "FIRST LINE"}}},
				{Items: []LineItem{{Text: "áéíóú ♪‘»«"}}},
			},
		},
		&Item{
			StartAt: 3 * time.Second,
			EndAt:   4 * time.Second,
			Lines: []Line{
				{Items: []LineItem{{
					Text:        "GREEN",
					InlineStyle: &StyleAttributes{TTMLColor: &Color{Green: 255}},
				}}},
				{Items: []LineItem{{
					Text:        "ITALIC",
					InlineStyle: &StyleAttributes{WebVTTItalics: true},
				}}},
				{Items: []LineItem{{
					Text:        "    UNDER",
					InlineStyle: &StyleAttributes{WebVTTUnderline: true},
				}}},
			},
		},
	)

	buf := &bytes.Buffer{}
	require.NoError(t, s.WriteToSCC(buf))
	out := buf.String()
	assert.True(t, strings.HasPrefix(out, sccHeader+"\r\n"))
	assert.Contains(t, out, "\r\n")
	sccCheckOddParity(t, out)

	// Round trip
	r, err := ReadFromSCC(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, r.Items, 2)
	frame := time.Second / 30

	require.Len(t, r.Items[0].Lines, 2)
	assert.Equal(t, "FIRST LINE", sccItemLineText(r.Items[0], 0))
	assert.Equal(t, "áéíóú ♪‘»«", sccItemLineText(r.Items[0], 1))
	assert.True(t, absDur(r.Items[0].StartAt-time.Second) <= frame+time.Millisecond)
	assert.True(t, absDur(r.Items[0].EndAt-2*time.Second) <= frame+time.Millisecond)

	require.Len(t, r.Items[1].Lines, 3)
	assert.Equal(t, "GREEN", sccItemLineText(r.Items[1], 0))
	style := r.Items[1].Lines[0].Items[0].InlineStyle
	require.NotNil(t, style)
	require.NotNil(t, style.TTMLColor)
	assert.Equal(t, uint8(0), style.TTMLColor.Red)
	assert.Equal(t, uint8(255), style.TTMLColor.Green)
	assert.Equal(t, uint8(0), style.TTMLColor.Blue)

	assert.Equal(t, "ITALIC", sccItemLineText(r.Items[1], 1))
	style = r.Items[1].Lines[1].Items[0].InlineStyle
	require.NotNil(t, style)
	assert.True(t, style.WebVTTItalics)

	assert.Equal(t, "    UNDER", sccItemLineText(r.Items[1], 2))
	style = r.Items[1].Lines[2].Items[0].InlineStyle
	require.NotNil(t, style)
	assert.True(t, style.WebVTTUnderline)

	assert.True(t, absDur(r.Items[1].StartAt-3*time.Second) <= frame+time.Millisecond)
	assert.True(t, absDur(r.Items[1].EndAt-4*time.Second) <= frame+time.Millisecond)
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func TestWriteToSCCLineLength(t *testing.T) {
	s := NewSubtitles()
	s.Items = append(s.Items, &Item{
		StartAt: time.Second,
		EndAt:   2 * time.Second,
		Lines:   []Line{{Items: []LineItem{{Text: strings.Repeat("A", 40) + "   "}}}},
	})
	buf := &bytes.Buffer{}
	require.NoError(t, s.WriteToSCC(buf))

	r, err := ReadFromSCC(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, r.Items, 1)
	assert.Equal(t, strings.Repeat("A", 32), sccItemLineText(r.Items[0], 0))
}

func TestSCCOpenWriteDispatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.scc")

	s := NewSubtitles()
	s.Items = append(s.Items, &Item{
		StartAt: time.Second,
		EndAt:   2 * time.Second,
		Lines:   []Line{{Items: []LineItem{{Text: "DISPATCHED"}}}},
	})
	require.NoError(t, s.Write(path))

	c, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(c, []byte(sccHeader)))

	r, err := OpenFile(path)
	require.NoError(t, err)
	require.Len(t, r.Items, 1)
	assert.Equal(t, "DISPATCHED", sccItemLineText(r.Items[0], 0))

	// Unknown extension still fails
	_, err = OpenFile(filepath.Join(dir, "example.unknown"))
	assert.Error(t, err)
}
