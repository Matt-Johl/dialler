package qr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// --- known vectors -----------------------------------------------------

func TestGFTables(t *testing.T) {
	cases := []struct{ i, want int }{{0, 1}, {1, 2}, {2, 4}, {7, 128}, {8, 29}, {9, 58}, {255, 1}}
	for _, c := range cases {
		if got := int(gfExp[c.i]); got != c.want {
			t.Errorf("exp(%d) = %d, want %d", c.i, got, c.want)
		}
	}
	for _, c := range []struct{ v, want int }{{1, 0}, {2, 1}, {4, 2}, {29, 8}, {128, 7}} {
		if got := int(gfLog[c.v]); got != c.want {
			t.Errorf("log(%d) = %d, want %d", c.v, got, c.want)
		}
	}
	for v := 1; v < 256; v++ {
		if gfExp[gfLog[v]] != byte(v) {
			t.Errorf("exp(log(%d)) = %d", v, gfExp[gfLog[v]])
		}
	}
	if gfMul(2, 128) != 29 { // α · α^7 = α^8 = 0x11D − 0x100
		t.Errorf("gfMul(2, 128) = %d, want 29", gfMul(2, 128))
	}
	// The table multiply against the decoder's shift-and-add multiply.
	for a := 0; a < 256; a += 7 {
		for b := 0; b < 256; b += 11 {
			if gfMul(byte(a), byte(b)) != refMul(byte(a), byte(b)) {
				t.Errorf("gfMul(%d, %d) = %d, refMul = %d", a, b, gfMul(byte(a), byte(b)), refMul(byte(a), byte(b)))
			}
		}
	}
}

func TestGeneratorPolynomial10(t *testing.T) {
	// ISO/IEC 18004 Annex A: the degree-10 generator polynomial as
	// exponents of α, from x^9 down to x^0.
	exps := []int{251, 67, 46, 61, 118, 70, 64, 94, 32, 45}
	got := rsGenerator(10)
	for i, e := range exps {
		if got[i] != gfExp[e] {
			t.Errorf("g[%d] = α^%d (%d), want α^%d (%d)", i, gfLog[got[i]], got[i], e, gfExp[e])
		}
	}
	// Degree 7 as a second point: exponents 87 229 146 149 238 102 21.
	got7 := rsGenerator(7)
	for i, e := range []int{87, 229, 146, 149, 238, 102, 21} {
		if got7[i] != gfExp[e] {
			t.Errorf("g7[%d] = α^%d, want α^%d", i, gfLog[got7[i]], e)
		}
	}
}

func TestReedSolomonVector(t *testing.T) {
	// The well-known 1-M "HELLO WORLD" example: 16 data codewords and
	// their 10 EC codewords.
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, rsGenerator(10)); !bytes.Equal(got, want) {
		t.Errorf("rsRemainder = %v, want %v", got, want)
	}
}

func TestFormatBits(t *testing.T) {
	want := []uint{0x5412, 0x5125, 0x5E7C, 0x5B4B, 0x45F9, 0x40CE, 0x4F97, 0x4AA0}
	for m, w := range want {
		if got := formatBits(m); got != w {
			t.Errorf("formatBits(M, %d) = %#06x, want %#06x", m, got, w)
		}
	}
}

func TestVersionBits(t *testing.T) {
	want := map[int]uint{7: 0x07C94, 8: 0x085BC, 9: 0x09A99, 10: 0x0A4D3}
	for v, w := range want {
		if got := versionBits(v); got != w {
			t.Errorf("versionBits(%d) = %#07x, want %#07x", v, got, w)
		}
	}
}

func TestByteModeSegment(t *testing.T) {
	// Empty input, v1: 0100 00000000 0000(terminator) → 0x40 0x00, then pads.
	got := encodeBytes(1, nil, 16)
	want := []byte{0x40, 0x00, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeBytes(1, \"\") = %x, want %x", got, want)
	}
	// "a", v1: 0100 00000001 01100001 0000 → 0x40 0x16 0x10, then pads.
	got = encodeBytes(1, []byte("a"), 16)
	if got[0] != 0x40 || got[1] != 0x16 || got[2] != 0x10 || got[3] != 0xEC || got[4] != 0x11 {
		t.Errorf("encodeBytes(1, \"a\") = %x", got)
	}
	// Full v1 (14 bytes): 4+8+112 = 124 bits, so the terminator is cut to 4
	// bits exactly and there are no pad codewords.
	full := encodeBytes(1, bytes.Repeat([]byte{0xFF}, 14), 16)
	if full[15] != 0xF0 {
		t.Errorf("full v1 last codeword = %#x, want 0xf0", full[15])
	}
	// v10 uses a 16-bit count.
	got = encodeBytes(10, []byte("a"), 216)
	if got[0] != 0x40 || got[1] != 0x00 || got[2] != 0x16 || got[3] != 0x10 {
		t.Errorf("encodeBytes(10, \"a\") = %x", got[:4])
	}
}

func TestTotalCodewordsPerVersion(t *testing.T) {
	total := []int{0, 26, 44, 70, 100, 134, 172, 196, 242, 292, 346}
	remainder := []int{0, 0, 7, 7, 7, 7, 7, 0, 0, 0, 0}
	for v := 1; v <= 10; v++ {
		g := newGrid(v)
		g.drawFunctionPatterns()
		if got := g.rawDataModules(); got != total[v]*8+remainder[v] {
			t.Errorf("v%d: raw data modules = %d, want %d (%d codewords + %d remainder bits)", v, got, total[v]*8+remainder[v], total[v], remainder[v])
		}
		ec := ecBlocks[v]
		dataCW := total[v] - ec.ecPerBlock*ec.blocks
		countBits := 8
		if v == 10 {
			countBits = 16
		}
		if capacity := (dataCW*8 - 4 - countBits) / 8; capacity != byteCapacity[v] {
			t.Errorf("v%d: derived byte capacity %d, table says %d", v, capacity, byteCapacity[v])
		}
	}
}

// --- structural tests --------------------------------------------------

func TestVersionSelection(t *testing.T) {
	for v := 1; v <= 10; v++ {
		c, err := Encode(bytes.Repeat([]byte("x"), byteCapacity[v]))
		if err != nil || c.Version != v {
			t.Errorf("%d bytes: version %v err %v, want v%d", byteCapacity[v], c, err, v)
		}
		if v > 1 {
			c, err := Encode(bytes.Repeat([]byte("x"), byteCapacity[v-1]+1))
			if err != nil || c.Version != v {
				t.Errorf("%d bytes: version %v err %v, want v%d", byteCapacity[v-1]+1, c, err, v)
			}
		}
	}
	for _, n := range []int{0, 14} {
		if c, _ := Encode(bytes.Repeat([]byte("x"), n)); c.Version != 1 || c.Size != 21 {
			t.Errorf("%d bytes → v%d size %d", n, c.Version, c.Size)
		}
	}
	if c, _ := Encode(bytes.Repeat([]byte("x"), 15)); c.Version != 2 || c.Size != 25 {
		t.Errorf("15 bytes → v%d", c.Version)
	}
	if c, _ := Encode(bytes.Repeat([]byte("x"), 213)); c.Version != 10 || c.Size != 57 {
		t.Errorf("213 bytes → v%d", c.Version)
	}
	if _, err := Encode(bytes.Repeat([]byte("x"), 214)); !errors.Is(err, ErrTooLong) {
		t.Errorf("214 bytes: err = %v, want ErrTooLong", err)
	}
}

func TestFunctionPatterns(t *testing.T) {
	for v := 1; v <= 10; v++ {
		c, err := Encode(bytes.Repeat([]byte("q"), byteCapacity[v]))
		if err != nil || c.Version != v {
			t.Fatalf("v%d: %v", v, err)
		}
		n := c.Size
		checkFinder := func(x0, y0 int) {
			for dy := 0; dy < 7; dy++ {
				for dx := 0; dx < 7; dx++ {
					ring := max(abs(dx-3), abs(dy-3))
					want := ring != 2
					if c.At(x0+dx, y0+dy) != want {
						t.Errorf("v%d finder at (%d,%d): module (%d,%d) = %v", v, x0, y0, x0+dx, y0+dy, !want)
					}
				}
			}
		}
		checkFinder(0, 0)
		checkFinder(n-7, 0)
		checkFinder(0, n-7)
		// Separators.
		for i := 0; i < 8; i++ {
			for _, p := range [][2]int{{7, i}, {i, 7}, {n - 8, i}, {n - 1 - i, 7}, {7, n - 1 - i}, {i, n - 8}} {
				if c.At(p[0], p[1]) {
					t.Errorf("v%d separator module (%d,%d) is dark", v, p[0], p[1])
				}
			}
		}
		// Timing patterns alternate, dark at even indices.
		for i := 8; i < n-8; i++ {
			if c.At(i, 6) != (i%2 == 0) || c.At(6, i) != (i%2 == 0) {
				t.Errorf("v%d timing at index %d wrong", v, i)
			}
		}
		// The dark module.
		if !c.At(8, n-8) {
			t.Errorf("v%d dark module missing", v)
		}
		// Alignment patterns.
		pos := alignPositions[v]
		for i, cy := range pos {
			for j, cx := range pos {
				if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
					continue
				}
				for dy := -2; dy <= 2; dy++ {
					for dx := -2; dx <= 2; dx++ {
						want := max(abs(dx), abs(dy)) != 1
						if c.At(cx+dx, cy+dy) != want {
							t.Errorf("v%d alignment at (%d,%d): module (%d,%d) = %v", v, cx, cy, cx+dx, cy+dy, !want)
						}
					}
				}
			}
		}
		if c.At(-1, 0) || c.At(0, n) {
			t.Errorf("v%d At outside the symbol is dark", v)
		}
	}
}

var svgRun = regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`)

func TestSVG(t *testing.T) {
	c, err := Encode([]byte("dialler://enrol?h=10.18.0.10&p=8080&c=8XK2M4PQ"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.SVG()
	if s != c.SVG() {
		t.Error("SVG is not deterministic")
	}
	n := c.Size + 8
	if !strings.HasPrefix(s, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `+strconv.Itoa(n)+" "+strconv.Itoa(n)+`"`) {
		t.Errorf("bad SVG prefix: %.80s", s)
	}
	if !strings.Contains(s, `shape-rendering="crispEdges"`) || !strings.Contains(s, `fill="#000"`) || !strings.Contains(s, `fill="#fff"`) {
		t.Error("SVG missing rendering attributes")
	}
	if root := s[:strings.Index(s, ">")]; strings.Contains(root, "width=") || strings.Contains(root, "height=") {
		t.Error("SVG root element sets width/height; CSS should size it")
	}
	if !strings.HasSuffix(s, `"/></svg>`) {
		t.Error("bad SVG suffix")
	}
	// Rebuild the module matrix from the path and compare with At.
	got := make([][]bool, n)
	for i := range got {
		got[i] = make([]bool, n)
	}
	dark := 0
	for _, m := range svgRun.FindAllStringSubmatch(s, -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		w2, _ := strconv.Atoi(m[4])
		if w != w2 {
			t.Fatalf("malformed run %q", m[0])
		}
		for i := 0; i < w; i++ {
			if got[y][x+i] {
				t.Fatalf("module (%d,%d) drawn twice", x+i, y)
			}
			got[y][x+i] = true
			dark++
		}
	}
	wantDark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			want := c.At(x-4, y-4)
			if want {
				wantDark++
			}
			if got[y][x] != want {
				t.Fatalf("SVG module (%d,%d) = %v, want %v", x, y, got[y][x], want)
			}
		}
	}
	if dark != wantDark {
		t.Errorf("SVG draws %d dark modules, symbol has %d", dark, wantDark)
	}
}

// --- decode round trip -------------------------------------------------

// The inputs used by the round trip, the goldens and the oracle export.
var roundTripInputs = []string{
	"",
	"a",
	"01234567890123",                 // 14 bytes, v1 boundary
	"012345678901234",                // 15 bytes, v2
	strings.Repeat("0123456789", 10), // 100 bytes, v6
	strings.Repeat("The quick brown fox jumps over the lazy dog. ", 4) + strings.Repeat("0123456789", 3) + "abc", // 213 bytes, v10
	"dialler://enrol?h=10.18.0.10&p=8080&c=8XK2M4PQ&f=-OzK0YvVPWjed4R_ebPLGzxRqzHzhm65SJiFwES2Bi4",
	"dialler://enrol?h=192.168.1.5&p=7443&c=Q1W2E3R4",
	"dialler://enrol?h=pbx.example.com&p=7443&c=ABCD1234&f=-OzK0YvVPWjed4R_ebPLGzxRqzHzhm65SJiFwES2Bi4&n=Reception+Desk",
	"dialler://enrol?h=2001:db8::10&p=7443&c=ZX9Q2K7M&f=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	strings.Repeat("v3-payload", 4) + "!!",  // 42 bytes, v3
	strings.Repeat("v5-block!", 8),          // 72 bytes, v5
	strings.Repeat("abcdefgh", 15) + "12",   // 122 bytes, v7
	strings.Repeat("ABCDEFGHIJ", 15) + "xy", // 152 bytes, v8
	strings.Repeat("0123456789", 18),        // 180 bytes, v9
}

func TestRoundTrip(t *testing.T) {
	seen := map[int]bool{}
	for _, in := range roundTripInputs {
		c, err := Encode([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		seen[c.Version] = true
		got, mask, err := refDecode(c.modules)
		if err != nil {
			t.Errorf("%q (v%d mask %d): decode: %v", in, c.Version, c.Mask, err)
			continue
		}
		if mask != c.Mask {
			t.Errorf("%q: decoded mask %d, Code.Mask %d", in, mask, c.Mask)
		}
		if string(got) != in {
			t.Errorf("%q: decoded %q", in, got)
		}
	}
	for v := 1; v <= 10; v++ {
		if !seen[v] {
			t.Errorf("round trip inputs do not cover v%d", v)
		}
	}
}

func TestRoundTripEveryLength(t *testing.T) {
	for n := 0; n <= MaxBytes; n++ {
		in := make([]byte, n)
		for i := range in {
			in[i] = byte('!' + (i*7+n)%94)
		}
		c, err := Encode(in)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		got, _, err := refDecode(c.modules)
		if err != nil {
			t.Fatalf("%d bytes (v%d mask %d): %v", n, c.Version, c.Mask, err)
		}
		if !bytes.Equal(got, in) {
			t.Fatalf("%d bytes: decoded %q", n, got)
		}
	}
}

// refDecode is an independent reference decoder for the subset this
// package produces. It uses its own copies of the standard's tables (the
// format and version information as listed in the standard, the block
// structure per version) and its own GF(256) arithmetic, so it does not
// share code with the encoder beyond the mask formulas themselves.
func refDecode(m [][]bool) (data []byte, mask int, err error) {
	size := len(m)
	version := (size - 17) / 4
	if version < 1 || version > 10 || size != 17+4*version {
		return nil, 0, fmt.Errorf("bad size %d", size)
	}
	at := func(x, y int) bool { return m[y][x] }

	// Format information, both copies; the standard's table for level M.
	formatTable := []uint{0x5412, 0x5125, 0x5E7C, 0x5B4B, 0x45F9, 0x40CE, 0x4F97, 0x4AA0}
	var f1, f2 uint
	for i := 14; i >= 0; i-- {
		var b1, b2 bool
		switch {
		case i <= 5:
			b1 = at(8, i)
		case i == 6:
			b1 = at(8, 7)
		case i == 7:
			b1 = at(8, 8)
		case i == 8:
			b1 = at(7, 8)
		default:
			b1 = at(14-i, 8)
		}
		if i < 8 {
			b2 = at(size-1-i, 8)
		} else {
			b2 = at(8, size-15+i)
		}
		f1 = f1<<1 | boolBit(b1)
		f2 = f2<<1 | boolBit(b2)
	}
	if f1 != f2 {
		return nil, 0, fmt.Errorf("format copies differ: %#x vs %#x", f1, f2)
	}
	mask = -1
	for i, v := range formatTable {
		if v == f1 {
			mask = i
		}
	}
	if mask < 0 {
		return nil, 0, fmt.Errorf("format %#x is not level M", f1)
	}
	if !at(8, size-8) {
		return nil, 0, errors.New("dark module is light")
	}

	// Version information for v7+, both copies, from the standard's table.
	if version >= 7 {
		versionTable := map[int]uint{7: 0x07C94, 8: 0x085BC, 9: 0x09A99, 10: 0x0A4D3}
		var v1, v2 uint
		for i := 17; i >= 0; i-- {
			v1 = v1<<1 | boolBit(at(size-11+i%3, i/3))
			v2 = v2<<1 | boolBit(at(i/3, size-11+i%3))
		}
		if v1 != versionTable[version] || v2 != versionTable[version] {
			return nil, 0, fmt.Errorf("version info %#x/%#x, want %#x", v1, v2, versionTable[version])
		}
	}

	fn := refFunctionMap(version)

	// Unmask and read the codewords in zigzag order.
	unmasked := func(x, y int) bool {
		var inv bool
		switch mask {
		case 0:
			inv = (x+y)%2 == 0
		case 1:
			inv = y%2 == 0
		case 2:
			inv = x%3 == 0
		case 3:
			inv = (x+y)%3 == 0
		case 4:
			inv = (x/3+y/2)%2 == 0
		case 5:
			inv = x*y%2+x*y%3 == 0
		case 6:
			inv = (x*y%2+x*y%3)%2 == 0
		case 7:
			inv = ((x+y)%2+x*y%3)%2 == 0
		}
		return at(x, y) != inv
	}
	var bits []bool
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		upward := ((right + 1) & 2) == 0
		for vert := 0; vert < size; vert++ {
			y := vert
			if upward {
				y = size - 1 - vert
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if !fn[y][x] {
					bits = append(bits, unmasked(x, y))
				}
			}
		}
	}
	total := len(bits) / 8
	cw := make([]byte, total)
	for i := 0; i < total*8; i++ {
		if bits[i] {
			cw[i/8] |= 0x80 >> uint(i%8)
		}
	}
	for _, b := range bits[total*8:] {
		if b {
			return nil, 0, errors.New("remainder bit is set")
		}
	}

	// Block structure for level M, ISO/IEC 18004 Table 9: per version, the
	// EC codewords per block and each block's data codeword count.
	blockTable := [11]struct {
		ec   int
		data []int
	}{
		{}, {10, []int{16}}, {16, []int{28}}, {26, []int{44}}, {18, []int{32, 32}}, {24, []int{43, 43}},
		{16, []int{27, 27, 27, 27}}, {18, []int{31, 31, 31, 31}}, {22, []int{38, 38, 39, 39}},
		{22, []int{36, 36, 36, 37, 37}}, {26, []int{43, 43, 43, 43, 44}},
	}
	bt := blockTable[version]
	sum := 0
	for _, d := range bt.data {
		sum += d + bt.ec
	}
	if sum != total {
		return nil, 0, fmt.Errorf("read %d codewords, table says %d", total, sum)
	}
	blocks := make([][]byte, len(bt.data))
	k := 0
	maxData := 0
	for _, d := range bt.data {
		maxData = max(maxData, d)
	}
	for col := 0; col < maxData; col++ {
		for i, d := range bt.data {
			if col < d {
				blocks[i] = append(blocks[i], cw[k])
				k++
			}
		}
	}
	for col := 0; col < bt.ec; col++ {
		for i := range blocks {
			blocks[i] = append(blocks[i], cw[k])
			k++
		}
	}
	// Every block's syndromes must be zero.
	for i, blk := range blocks {
		for s := 0; s < bt.ec; s++ {
			alpha := refPow(2, s)
			var acc byte
			for _, b := range blk {
				acc = refMul(acc, alpha) ^ b
			}
			if acc != 0 {
				return nil, 0, fmt.Errorf("block %d syndrome %d = %#x", i, s, acc)
			}
		}
	}
	var payload []byte
	for i, blk := range blocks {
		payload = append(payload, blk[:bt.data[i]]...)
	}

	// Byte mode segment.
	r := bitReader{b: payload}
	if mode := r.read(4); mode != 4 {
		return nil, 0, fmt.Errorf("mode indicator %04b", mode)
	}
	countBits := 8
	if version == 10 {
		countBits = 16
	}
	n := r.read(countBits)
	data = make([]byte, n)
	for i := range data {
		data[i] = byte(r.read(8))
	}
	// Terminator (up to 4 zero bits), zero fill to the byte boundary, then
	// alternating 0xEC/0x11 pad codewords.
	for i := 0; i < 4 && r.pos < len(payload)*8; i++ {
		if r.read(1) != 0 {
			return nil, 0, errors.New("terminator bit set")
		}
	}
	for r.pos%8 != 0 {
		if r.read(1) != 0 {
			return nil, 0, errors.New("fill bit set")
		}
	}
	for pad := 0xEC; r.pos < len(payload)*8; pad ^= 0xEC ^ 0x11 {
		if got := r.read(8); got != pad {
			return nil, 0, fmt.Errorf("pad codeword %#x, want %#x", got, pad)
		}
	}
	return data, mask, nil
}

// refFunctionMap marks every module that does not carry data: finders
// with separators and format areas, timing patterns, alignment patterns,
// the dark module and the version information areas.
func refFunctionMap(version int) [][]bool {
	size := 17 + 4*version
	fn := make([][]bool, size)
	for i := range fn {
		fn[i] = make([]bool, size)
	}
	mark := func(x0, y0, x1, y1 int) {
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				fn[y][x] = true
			}
		}
	}
	mark(0, 0, 8, 8)           // top-left finder, separator, format
	mark(size-8, 0, size-1, 8) // top-right finder, separator, format row
	mark(0, size-8, 8, size-1) // bottom-left finder, separator, format column
	mark(6, 0, 6, size-1)      // vertical timing
	mark(0, 6, size-1, 6)      // horizontal timing
	align := [11][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}[version]
	for i, cy := range align {
		for j, cx := range align {
			if (i == 0 && j == 0) || (i == 0 && j == len(align)-1) || (i == len(align)-1 && j == 0) {
				continue
			}
			mark(cx-2, cy-2, cx+2, cy+2)
		}
	}
	if version >= 7 {
		mark(size-11, 0, size-9, 5)
		mark(0, size-11, 5, size-9)
	}
	return fn
}

// refMul multiplies in GF(2^8)/0x11D by shift-and-add, independently of
// the encoder's tables.
func refMul(a, b byte) byte {
	var p byte
	for b != 0 {
		if b&1 != 0 {
			p ^= a
		}
		carry := a & 0x80
		a <<= 1
		if carry != 0 {
			a ^= 0x1D
		}
		b >>= 1
	}
	return p
}

func refPow(a byte, n int) byte {
	r := byte(1)
	for i := 0; i < n; i++ {
		r = refMul(r, a)
	}
	return r
}

type bitReader struct {
	b   []byte
	pos int
}

func (r *bitReader) read(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		bit := (r.b[r.pos/8] >> uint(7-r.pos%8)) & 1
		v = v<<1 | int(bit)
		r.pos++
	}
	return v
}

func boolBit(b bool) uint {
	if b {
		return 1
	}
	return 0
}

// --- goldens -----------------------------------------------------------

func rows(c *Code) []string {
	out := make([]string, c.Size)
	for y := 0; y < c.Size; y++ {
		var sb strings.Builder
		for x := 0; x < c.Size; x++ {
			if c.At(x, y) {
				sb.WriteByte('#')
			} else {
				sb.WriteByte('.')
			}
		}
		out[y] = sb.String()
	}
	return out
}

// TestGolden checks Encode still produces exactly the matrices that the
// external oracle (zbar) decoded correctly, which also pins the mask choice.
func TestGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string][]string
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) == 0 {
		t.Fatal("golden.json is empty")
	}
	for _, in := range roundTripInputs {
		want, ok := golden[in]
		if !ok {
			t.Errorf("no golden for %q", in)
			continue
		}
		c, err := Encode([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		got := rows(c)
		if len(got) != len(want) {
			t.Errorf("%q: %d rows, golden has %d", in, len(got), len(want))
			continue
		}
		for y := range got {
			if got[y] != want[y] {
				t.Errorf("%q: row %d\n got %s\nwant %s", in, y, got[y], want[y])
				break
			}
		}
	}
}

// TestOracleExport writes each round-trip input as a PNG (10 px per
// module, with the quiet zone) plus golden.json into $QR_ORACLE_DIR, for
// decoding with an external reader. It is skipped unless that variable is
// set; the goldens in testdata are the output of a run whose PNGs zbar
// decoded correctly.
func TestOracleExport(t *testing.T) {
	dir := os.Getenv("QR_ORACLE_DIR")
	if dir == "" {
		t.Skip("QR_ORACLE_DIR not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	golden := map[string][]string{}
	for i, in := range roundTripInputs {
		c, err := Encode([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		golden[in] = rows(c)
		const px = 10
		n := (c.Size + 8) * px
		img := image.NewGray(image.Rect(0, 0, n, n))
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				v := color.Gray{Y: 255}
				if c.At(x/px-4, y/px-4) {
					v = color.Gray{Y: 0}
				}
				img.SetGray(x, y, v)
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("%02d_v%d_m%d.png", i, c.Version, c.Mask)
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d.txt", i)), []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	js, err := json.MarshalIndent(golden, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "golden.json"), js, 0o644); err != nil {
		t.Fatal(err)
	}
}
