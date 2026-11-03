package astisub

import (
	"bufio"
	"fmt"
	"io"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scenarist SCC (CEA-608 closed captions)
const (
	sccHeader        = "Scenarist_SCC V1.0"
	sccFramerate     = 30
	sccMaxLineLength = 32
	sccMaxRows       = 15
)

// CEA-608 control codes (parity stripped values, first byte is always 0x14 on
// channel 1)
const (
	sccCtrlRCL byte = 0x20 // Resume Caption Loading (start pop-on loading)
	sccCtrlRU2 byte = 0x25 // Roll-up 2 rows
	sccCtrlRU3 byte = 0x26 // Roll-up 3 rows
	sccCtrlRU4 byte = 0x27 // Roll-up 4 rows
	sccCtrlRDC byte = 0x29 // Resume Direct Captioning (paint-on)
	sccCtrlEDM byte = 0x2c // Erase Displayed Memory
	sccCtrlCR  byte = 0x2d // Carriage Return
	sccCtrlENM byte = 0x2e // Erase Non-displayed Memory
	sccCtrlEOC byte = 0x2f // End Of Caption (swap memories)
)

// First byte (parity stripped) of a PAC for a row pair. Each entry gives the
// two rows addressed by second bytes 0x40-0x5f and 0x60-0x7f respectively.
var sccPACRowByFirstByte = [8][2]int{
	{11, -1}, // 0x10: row 11 only (second byte 0x40-0x5f)
	{1, 2},   // 0x11
	{3, 4},   // 0x12
	{12, 13}, // 0x13
	{14, 15}, // 0x14
	{5, 6},   // 0x15
	{7, 8},   // 0x16
	{9, 10},  // 0x17
}

// Row -> PAC first byte and second byte base, used by the writer
var sccRowPAC = [16]struct{ first, base byte }{
	1:  {0x11, 0x40},
	2:  {0x11, 0x60},
	3:  {0x12, 0x40},
	4:  {0x12, 0x60},
	5:  {0x15, 0x40},
	6:  {0x15, 0x60},
	7:  {0x16, 0x40},
	8:  {0x16, 0x60},
	9:  {0x17, 0x40},
	10: {0x17, 0x60},
	11: {0x10, 0x40},
	12: {0x13, 0x40},
	13: {0x13, 0x60},
	14: {0x14, 0x40},
	15: {0x14, 0x60},
}

// CEA-608 PAC colors in low-bits order: white is not stored as a color.
var sccPACColors = [7]*Color{
	nil,
	{Green: 255},            // green  #00FF00
	{Blue: 255},             // blue   #0000FF
	{Green: 255, Blue: 255}, // cyan   #00FFFF
	{Red: 255},              // red    #FF0000
	{Red: 255, Green: 255},  // yellow #FFFF00
	{Red: 255, Blue: 255},   // magenta #FF00FF
}

// Basic North American character set (parity stripped bytes 0x20-0x7f),
// mostly ASCII with the CEA-608 differences.
var sccBasicChars = [0x80]rune{}

// Special North American characters: (0x11, second byte 0x30-0x3f)
var sccSpecialChars = [16]rune{
	'®', '°', '½', '¿', '™', '¢', '£', '♪',
	'à', ' ', 'è', 'â', 'ê', 'î', 'ô', 'û',
}

// Extended Western European characters: first byte 0x12/0x13, second byte
// 0x20-0x3f (same table as CEA-608).
var sccExtendedChars = [2][32]rune{
	{ // 0x12
		'Á', 'É', 'Ó', 'Ú', 'Ü', 'ü', '´', '¡',
		'*', '‘', '-', '©', '℠', '·', '“', '”',
		'À', 'Â', 'Ç', 'È', 'Ê', 'Ë', 'ë', 'Î',
		'Ï', 'ï', 'Ô', 'Ù', 'ù', 'Û', '«', '»',
	},
	{ // 0x13
		'Ã', 'ã', 'Í', 'Ì', 'ì', 'Ò', 'ò', 'Õ',
		'õ', '{', '}', '\\', '^', '_', '|', '~',
		'Ä', 'ä', 'Ö', 'ö', 'ß', '¥', '¤', '¦',
		'Å', 'å', 'Ø', 'ø', '┌', '┐', '└', '┘',
	},
}

// Reverse lookup tables, built in init
var (
	sccEncodeBasic    map[rune]byte
	sccEncodeSpecial  map[rune][2]byte
	sccEncodeExtended map[rune][2]byte
)

func init() {
	// Basic set: ASCII 0x20-0x7e
	for b := 0x20; b <= 0x7e; b++ {
		sccBasicChars[b] = rune(b)
	}
	// CEA-608 specific positions
	for b, r := range map[byte]rune{
		0x2a: 'á',
		0x5c: 'é',
		0x5e: 'í',
		0x5f: 'ó',
		0x60: 'ú',
		0x7b: 'ç',
		0x7c: '÷',
		0x7d: 'Ñ',
		0x7e: 'ñ',
		0x7f: '█',
	} {
		sccBasicChars[b] = r
	}

	// Reverse maps
	sccEncodeBasic = make(map[rune]byte)
	sccEncodeSpecial = make(map[rune][2]byte)
	sccEncodeExtended = make(map[rune][2]byte)
	for b := 0x20; b <= 0x7f; b++ {
		if r := sccBasicChars[b]; r != 0 {
			sccEncodeBasic[r] = byte(b)
		}
	}
	for i, r := range sccSpecialChars {
		if r == ' ' {
			// Transparent space is encoded as a regular space on write.
			continue
		}
		if _, ok := sccEncodeSpecial[r]; !ok {
			sccEncodeSpecial[r] = [2]byte{0x11, byte(0x30 + i)}
		}
	}
	for row := 0; row < 2; row++ {
		for i, r := range sccExtendedChars[row] {
			if r == 0 {
				continue
			}
			if _, okBasic := sccEncodeBasic[r]; okBasic {
				// Characters identical to ASCII/basic are written with one byte.
				continue
			}
			if _, okSpecial := sccEncodeSpecial[r]; okSpecial {
				continue
			}
			if _, ok := sccEncodeExtended[r]; !ok {
				sccEncodeExtended[r] = [2]byte{byte(0x12 + row), byte(0x20 + i)}
			}
		}
	}
}

// withOddParity sets the 7th bit so that the byte carries odd parity
func sccWithOddParity(b byte) byte {
	b &= 0x7f
	if bits.OnesCount8(b)%2 == 0 {
		b |= 0x80
	}
	return b
}

// sccWord formats two parity-stripped bytes as a four hex digits code word
func sccWord(a, b byte) string {
	return fmt.Sprintf("%02X%02X", sccWithOddParity(a), sccWithOddParity(b))
}

// parseSCCTimecode parses a HH:MM:SS:FF (non drop frame) or HH:MM:SS;FF
// (drop frame) timecode.
func parseSCCTimecode(tc string) (d time.Duration, err error) {
	if len(tc) != 11 {
		err = fmt.Errorf("astisub: invalid scc timecode %s", tc)
		return
	}
	for _, i := range []int{0, 1, 3, 4, 6, 7, 9, 10} {
		if tc[i] < '0' || tc[i] > '9' {
			err = fmt.Errorf("astisub: invalid scc timecode %s", tc)
			return
		}
	}
	for _, i := range []int{2, 5, 8} {
		if tc[i] != ':' && tc[i] != ';' {
			err = fmt.Errorf("astisub: invalid scc timecode %s", tc)
			return
		}
	}
	h := int(tc[0]-'0')*10 + int(tc[1]-'0')
	m := int(tc[3]-'0')*10 + int(tc[4]-'0')
	s := int(tc[6]-'0')*10 + int(tc[7]-'0')
	f := int(tc[9]-'0')*10 + int(tc[10]-'0')
	if m >= 60 || s >= 60 || f >= 30 {
		err = fmt.Errorf("astisub: invalid scc timecode %s", tc)
		return
	}

	frames := ((h*60+m)*60+s)*30 + f
	if tc[8] == ';' {
		// Drop frame: frame numbers 0 and 1 are skipped at the start of every
		// minute except every 10th minute.
		totalMinutes := h*60 + m
		frames -= 2 * (totalMinutes - totalMinutes/10)
		// 30000/1001 frames per second, rounded to the nearest millisecond
		d = time.Duration((int64(frames)*1001+15)/30) * time.Millisecond
		return
	}
	// Non drop frame: 30 frames per second
	d = time.Duration(frames) * time.Second / 30
	return
}

// formatSCCTimecode turns a duration into a non drop frame colon timecode
func formatSCCTimecode(d time.Duration) string {
	frames := int((int64(d)*30 + int64(time.Second)/2) / int64(time.Second))
	if frames < 0 {
		frames = 0
	}
	h := frames / 108000
	frames %= 108000
	m := frames / 1800
	frames %= 1800
	s := frames / 30
	f := frames % 30
	return fmt.Sprintf("%02d:%02d:%02d:%02d", h, m, s, f)
}

// pacStyle turns the five PAC low bits into a style and an indentation
func sccPACStyle(low byte) (style *StyleAttributes, indent int) {
	underline := low&1 == 1
	switch {
	case low <= 0x0d:
		color := sccPACColors[int(low)/2]
		if color == nil && !underline {
			return nil, 0
		}
		style = &StyleAttributes{}
		style.TTMLColor = color
		if underline {
			style.WebVTTUnderline = true
		}
		return style, 0
	case low == 0x0e:
		return &StyleAttributes{WebVTTItalics: true}, 0
	case low == 0x0f:
		return &StyleAttributes{WebVTTItalics: true, WebVTTUnderline: true}, 0
	default: // 0x10-0x1f: indentation
		indent = (int(low) - 0x10) / 2 * 4
		if underline {
			return &StyleAttributes{WebVTTUnderline: true}, indent
		}
		return nil, indent
	}
}

// sccPACLow turns line attributes into the five PAC low bits. It also returns
// the number of leading spaces represented by a PAC indentation, so that they
// are not emitted as character bytes.
func sccPACLow(c *Color, italic, underline bool, leadingSpaces int) (low byte, indentCols int) {
	ul := func(b byte) byte {
		if underline {
			return b | 1
		}
		return b
	}
	if c != nil {
		switch uint32(c.Red)<<16 | uint32(c.Green)<<8 | uint32(c.Blue) {
		case 0x00ff00:
			low = ul(0x02)
		case 0x0000ff:
			low = ul(0x04)
		case 0x00ffff:
			low = ul(0x06)
		case 0xff0000:
			low = ul(0x08)
		case 0xffff00:
			low = ul(0x0a)
		case 0xff00ff:
			low = ul(0x0c)
		default:
			// White or unknown color
			low = ul(0x00)
		}
		return
	}
	if italic {
		low = ul(0x0e)
		return
	}
	cols := leadingSpaces / 4 * 4
	if cols > 28 {
		cols = 28
	}
	if cols == 0 && !underline {
		return 0x00, 0
	}
	low = ul(byte(0x10 + cols/4*2))
	return low, cols
}

// sccLineBuf is a caption memory row being built
type sccLineBuf struct {
	runes  []rune
	style  *StyleAttributes
	indent int
}

type sccMode int

const (
	sccModePopOn sccMode = iota
	sccModeRollUp
	sccModePaintOn
)

type sccDecoder struct {
	subs       *Subtitles
	mode       sccMode
	hidden     map[int]*sccLineBuf
	displayed  map[int]*sccLineBuf
	row        int
	pop        *Item // caption currently displayed since the last EOC
	rollStart  time.Duration
	paintStart time.Duration
	lastT      time.Duration

	// Duplicate control code detection
	lastCtrl bool
	lastA    byte
	lastB    byte
}

func newSCCDecoder() *sccDecoder {
	return &sccDecoder{
		hidden:    make(map[int]*sccLineBuf),
		displayed: make(map[int]*sccLineBuf),
		row:       -1,
	}
}

func (d *sccDecoder) target() map[int]*sccLineBuf {
	if d.mode == sccModePopOn {
		return d.hidden
	}
	return d.displayed
}

func (d *sccDecoder) addItem(i *Item) {
	if len(i.Lines) > 0 {
		d.subs.Items = append(d.subs.Items, i)
	}
}

// buildLines turns a caption memory into ordered subtitle lines
func (d *sccDecoder) buildLines(m map[int]*sccLineBuf) []Line {
	rows := make([]int, 0, len(m))
	for r, lb := range m {
		if lb != nil && strings.TrimRight(string(lb.runes), " ") != "" {
			rows = append(rows, r)
		}
	}
	sort.Ints(rows)
	var lines []Line
	for _, r := range rows {
		lb := m[r]
		text := strings.TrimRight(string(lb.runes), " ")
		if lb.indent > 0 {
			text = strings.Repeat(" ", lb.indent) + text
		}
		li := LineItem{Text: text}
		if lb.style != nil {
			li.InlineStyle = lb.style
		}
		lines = append(lines, Line{Items: []LineItem{li}})
	}
	return lines
}

func (d *sccDecoder) writeRune(r rune) {
	if d.row < 0 {
		return
	}
	m := d.target()
	lb := m[d.row]
	if lb == nil {
		lb = &sccLineBuf{}
		m[d.row] = lb
	}
	lb.runes = append(lb.runes, r)
}

func (d *sccDecoder) backspace() {
	if d.row < 0 {
		return
	}
	if lb := d.target()[d.row]; lb != nil && len(lb.runes) > 0 {
		lb.runes = lb.runes[:len(lb.runes)-1]
	}
}

func (d *sccDecoder) pac(row int, low byte) {
	style, indent := sccPACStyle(low)
	m := d.target()
	if d.mode == sccModeRollUp {
		// In roll-up mode addressing a row erases it: the base row is always
		// blank when a new caption line starts being written.
		m[row] = &sccLineBuf{style: style, indent: indent}
	} else {
		lb := m[row]
		if lb == nil {
			lb = &sccLineBuf{}
			m[row] = lb
		}
		lb.style = style
		lb.indent = indent
	}
	d.row = row
}

func (d *sccDecoder) emitScreen(startAt, endAt time.Duration) {
	lines := d.buildLines(d.displayed)
	if len(lines) == 0 {
		return
	}
	d.addItem(&Item{StartAt: startAt, EndAt: endAt, Lines: lines})
}

func (d *sccDecoder) rcl() {
	d.mode = sccModePopOn
	d.row = -1
}

func (d *sccDecoder) enm() {
	d.hidden = make(map[int]*sccLineBuf)
	d.row = -1
}

func (d *sccDecoder) eoc(t time.Duration) {
	// A new pop-on caption supersedes the one currently displayed.
	if d.pop != nil {
		d.pop.EndAt = t
		d.addItem(d.pop)
		d.pop = nil
	}
	// Swap memories: the loaded content gets displayed.
	d.displayed, d.hidden = d.hidden, d.displayed
	d.row = -1
	d.pop = &Item{StartAt: t, Lines: d.buildLines(d.displayed)}
}

func (d *sccDecoder) edm(t time.Duration) {
	switch d.mode {
	case sccModePopOn:
		if d.pop != nil {
			d.pop.EndAt = t
			d.addItem(d.pop)
			d.pop = nil
		}
	case sccModeRollUp:
		d.emitScreen(d.rollStart, t)
	case sccModePaintOn:
		d.emitScreen(d.paintStart, t)
	}
	d.displayed = make(map[int]*sccLineBuf)
	d.row = -1
	d.mode = sccModePopOn
}

func (d *sccDecoder) rollUp(size int, t time.Duration) {
	if d.mode != sccModeRollUp {
		d.mode = sccModeRollUp
		d.rollStart = t
		d.row = -1
	}
}

func (d *sccDecoder) carriageReturn(t time.Duration) {
	if d.mode != sccModeRollUp {
		return
	}
	// Output everything currently on screen.
	d.emitScreen(d.rollStart, t)

	// Scroll the whole screen up one physical row: every row moves to the
	// previous row number, the top row disappears and the base row (15) is
	// cleared and becomes the cursor position.
	next := make(map[int]*sccLineBuf)
	for r := 2; r <= sccMaxRows; r++ {
		if lb := d.displayed[r]; lb != nil {
			next[r-1] = lb
		}
	}
	d.displayed = next
	d.row = sccMaxRows
	d.rollStart = t
}

func (d *sccDecoder) rdc(t time.Duration) {
	d.mode = sccModePaintOn
	d.paintStart = t
	d.row = -1
}

// feed processes one parity stripped code word
func (d *sccDecoder) feed(a, b byte, t time.Duration) {
	d.lastT = t

	// Filler pair (8080)
	if a == 0x00 {
		d.lastCtrl = false
		return
	}

	// Preamble address codes (PAC, second byte 0x40-0x7f) and control
	// commands (only on first byte 0x14, second byte below 0x40) share the
	// same 0x10-0x17 first byte range and are both sent twice in a row; the
	// duplicate only takes effect once.
	if a >= 0x10 && a <= 0x17 && (b >= 0x40 || a == 0x14) && b <= 0x7f {
		if d.lastCtrl && d.lastA == a && d.lastB == b {
			return
		}
		d.lastCtrl = true
		d.lastA, d.lastB = a, b

		// PAC rows 14/15 use the 0x14 prefix, same as control commands.
		if b >= 0x40 {
			pair := 0
			if b&0x60 == 0x60 {
				pair = 1
			}
			row := sccPACRowByFirstByte[a-0x10][pair]
			if row < 0 {
				return
			}
			d.pac(row, b&0x1f)
			return
		}

		switch b {
		case sccCtrlRCL:
			d.rcl()
		case sccCtrlENM:
			d.enm()
		case sccCtrlEOC:
			d.eoc(t)
		case sccCtrlEDM:
			d.edm(t)
		case sccCtrlCR:
			d.carriageReturn(t)
		case sccCtrlRU2:
			d.rollUp(2, t)
		case sccCtrlRU3:
			d.rollUp(3, t)
		case sccCtrlRU4:
			d.rollUp(4, t)
		case sccCtrlRDC:
			d.rdc(t)
		}
		return
	}

	if a >= 0x10 && a <= 0x17 {
		// Special and extended characters are printable data, not controls,
		// so two identical words in a row carry two characters.
		if a == 0x11 && b >= 0x30 && b <= 0x3f {
			d.lastCtrl = false
			d.writeRune(sccSpecialChars[b-0x30])
			return
		}
		if (a == 0x12 || a == 0x13) && b >= 0x20 && b <= 0x3f {
			d.lastCtrl = false
			// Extended character: delete the previous written character
			// first.
			d.backspace()
			if r := sccExtendedChars[a-0x12][b-0x20]; r != 0 {
				d.writeRune(r)
			}
			return
		}
		d.lastCtrl = false
		return
	}

	d.lastCtrl = false
	// Channel 2 prefixes and other odd values are ignored.
	if a >= 0x20 && a <= 0x7f {
		d.writeRune(sccBasicChars[a])
		if b != 0x00 {
			if b >= 0x20 && b <= 0x7f {
				d.writeRune(sccBasicChars[b])
			}
		}
	}
}

// finish flushes captions that were not explicitly closed at EOF
func (d *sccDecoder) finish() {
	switch d.mode {
	case sccModePopOn:
		if d.pop != nil {
			d.pop.EndAt = d.lastT
			d.addItem(d.pop)
			d.pop = nil
		}
	case sccModeRollUp:
		d.emitScreen(d.rollStart, d.lastT)
	case sccModePaintOn:
		d.emitScreen(d.paintStart, d.lastT)
	}
}

// ReadFromSCC parses a Scenarist .scc content
func ReadFromSCC(i io.Reader) (o *Subtitles, err error) {
	o = NewSubtitles()
	o.Metadata = &Metadata{Framerate: sccFramerate}
	var scanner = newScanner(i)
	d := newSCCDecoder()
	d.subs = o

	headerOK := false
	firstLine := true
	lineNum := 0
	for scanner.Scan() {
		line := scanner.Text()
		lineNum++
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !headerOK {
			if line != sccHeader {
				err = fmt.Errorf("astisub: line %d: invalid scc header %q", lineNum, line)
				return
			}
			headerOK = true
			continue
		}

		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			err = fmt.Errorf("astisub: line %d: missing tab separator", lineNum)
			return
		}
		var t time.Duration
		if t, err = parseSCCTimecode(line[:tab]); err != nil {
			err = fmt.Errorf("astisub: line %d: %w", lineNum, err)
			return
		}

		// Duplicate control pairs only occur within the same timecode row.
		d.lastCtrl = false
		for _, tok := range strings.Fields(line[tab+1:]) {
			if len(tok) != 4 {
				err = fmt.Errorf("astisub: line %d: invalid scc code word %q", lineNum, tok)
				return
			}
			var v uint64
			if v, err = strconv.ParseUint(tok, 16, 16); err != nil {
				err = fmt.Errorf("astisub: line %d: invalid scc code word %q", lineNum, tok)
				return
			}
			b1, b2 := byte(v>>8), byte(v)
			if bits.OnesCount8(b1)%2 == 0 || bits.OnesCount8(b2)%2 == 0 {
				err = fmt.Errorf("astisub: line %d: invalid odd parity in code word %q", lineNum, tok)
				return
			}
			d.feed(b1&0x7f, b2&0x7f, t)
		}
	}
	if err = scanner.Err(); err != nil {
		err = fmt.Errorf("astisub: scanning scc content failed: %w", err)
		return
	}
	if !headerOK {
		err = fmt.Errorf("astisub: invalid scc header")
		return
	}
	d.finish()
	return
}

// sccPairer assembles single and double bytes CEA-608 code words
type sccPairer struct {
	tokens  []string
	pending int // -1 when no single byte is waiting
}

func newSCCPairer() *sccPairer {
	return &sccPairer{pending: -1}
}

func (p *sccPairer) single(b byte) {
	if p.pending < 0 {
		p.pending = int(b)
		return
	}
	p.tokens = append(p.tokens, sccWord(byte(p.pending), b))
	p.pending = -1
}

// double emits a two-byte code (special character), padding a pending
// single byte first so the control byte is the first byte.
func (p *sccPairer) double(a, b byte) {
	if p.pending >= 0 {
		p.tokens = append(p.tokens, sccWord(byte(p.pending), 0x00))
		p.pending = -1
	}
	p.tokens = append(p.tokens, sccWord(a, b))
}

// extended emits a Western European extended character. On reception such a
// code deletes the previously written character and writes the new one, so a
// sacrificial space is emitted right before it: the space gets deleted and
// the extended character takes its place, leaving the preceding real
// character untouched.
func (p *sccPairer) extended(a, b byte) {
	if p.pending >= 0 {
		p.tokens = append(p.tokens, sccWord(byte(p.pending), 0x20))
		p.pending = -1
	} else {
		p.tokens = append(p.tokens, sccWord(0x20, 0x00))
	}
	p.tokens = append(p.tokens, sccWord(a, b))
}

func (p *sccPairer) end() {
	if p.pending >= 0 {
		p.tokens = append(p.tokens, sccWord(byte(p.pending), 0x00))
		p.pending = -1
	}
}

func (p *sccPairer) rune_(r rune) {
	if b, ok := sccEncodeBasic[r]; ok {
		p.single(b)
		return
	}
	if db, ok := sccEncodeSpecial[r]; ok {
		p.double(db[0], db[1])
		return
	}
	if db, ok := sccEncodeExtended[r]; ok {
		p.extended(db[0], db[1])
		return
	}
	// Unknown character is replaced by a question mark.
	p.single('?')
}

// sccLineText returns the usable text of a line after trailing space removal
// and 32 characters truncation. An empty string means the line is skipped.
func sccLineText(line Line) string {
	var text string
	for _, it := range line.Items {
		text += it.Text
	}
	text = strings.TrimRight(text, " ")
	runes := []rune(text)
	if len(runes) > sccMaxLineLength {
		text = string(runes[:sccMaxLineLength])
	}
	return text
}

// encodeSCCLine encodes one subtitle line for a given CEA-608 row.
func encodeSCCLine(row int, line Line, text string) []string {
	runes := []rune(text)
	leading := 0
	for leading < len(runes) && runes[leading] == ' ' {
		leading++
	}

	var color *Color
	var italic, underline bool
	for _, it := range line.Items {
		if it.InlineStyle != nil {
			color = it.InlineStyle.TTMLColor
			italic = it.InlineStyle.WebVTTItalics
			underline = it.InlineStyle.WebVTTUnderline
			break
		}
	}
	low, indentCols := sccPACLow(color, italic, underline, leading)

	info := sccRowPAC[row]
	pac := sccWord(info.first, info.base+low)
	tokens := []string{pac, pac}

	p := newSCCPairer()
	for i := 0; i < leading-indentCols; i++ {
		p.single(0x20)
	}
	for _, r := range runes[leading:] {
		p.rune_(r)
	}
	p.end()
	return append(tokens, p.tokens...)
}

// WriteToSCC writes content in the Scenarist .scc format using pop-on
// captions, non drop frame timecodes at 30 frames per second.
func (s Subtitles) WriteToSCC(o io.Writer) (err error) {
	if len(s.Items) == 0 {
		err = ErrNoSubtitlesToWrite
		return
	}

	var cw = bufio.NewWriter(o)
	defer func() {
		if err == nil {
			err = cw.Flush()
		}
	}()

	if _, err = cw.WriteString(sccHeader + "\r\n\r\n"); err != nil {
		return
	}

	written := 0
	for _, item := range s.Items {
		// Collect usable lines, up to 15, bottom anchored.
		type usable struct {
			line Line
			text string
		}
		var lines []usable
		for _, line := range item.Lines {
			if text := sccLineText(line); text != "" {
				lines = append(lines, usable{line: line, text: text})
			}
		}
		if len(lines) > sccMaxRows {
			lines = lines[len(lines)-sccMaxRows:]
		}
		if len(lines) == 0 {
			continue
		}
		firstRow := sccMaxRows + 1 - len(lines)

		var startTokens []string
		// RCL RCL ENM ENM
		startTokens = append(startTokens, sccWord(0x14, sccCtrlRCL), sccWord(0x14, sccCtrlRCL))
		startTokens = append(startTokens, sccWord(0x14, sccCtrlENM), sccWord(0x14, sccCtrlENM))
		for i, l := range lines {
			startTokens = append(startTokens, encodeSCCLine(firstRow+i, l.line, l.text)...)
		}
		// EOC EOC
		startTokens = append(startTokens, sccWord(0x14, sccCtrlEOC), sccWord(0x14, sccCtrlEOC))

		if _, err = cw.WriteString(formatSCCTimecode(item.StartAt) + "\t" + strings.Join(startTokens, " ") + "\r\n"); err != nil {
			return
		}
		// EDM EDM
		if _, err = cw.WriteString(formatSCCTimecode(item.EndAt) + "\t" + sccWord(0x14, sccCtrlEDM) + " " + sccWord(0x14, sccCtrlEDM) + "\r\n\r\n"); err != nil {
			return
		}
		written++
	}
	if written == 0 {
		err = ErrNoSubtitlesToWrite
	}
	return
}
