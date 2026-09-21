// Package qr encodes a byte string as a QR code (ISO/IEC 18004), for the
// enrolment link the admin UI shows (SPEC §4.8, §6 item 9). Byte mode,
// error-correction level M, versions 1–10 (up to 213 bytes), which covers
// a dialler://enrol link several times over. Standard library only, so the
// admin binary builds offline like the call server.
//
// The layout follows the specification's order: data codewords, Reed–Solomon
// blocks, interleaving, function patterns, zigzag placement, the eight
// masks scored by the four penalty rules, format and (from version 7)
// version information. Each piece is pinned by a known vector in the tests.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// ErrTooLong is returned when data does not fit version 10 at level M.
var ErrTooLong = errors.New("qr: data too long for version 10-M (213 bytes)")

// Code is an encoded symbol: Size modules a side, dark modules true.
type Code struct {
	Version int
	Size    int
	modules []bool
}

// Dark reports whether the module at column x, row y is dark.
func (c *Code) Dark(x, y int) bool { return c.modules[y*c.Size+x] }

// SVG renders the symbol as an inline SVG element, one unit per module,
// with a quiet zone of four modules, in the current text colour so the
// page's stylesheet decides how it looks.
func (c *Code) SVG() string {
	const quiet = 4
	total := c.Size + 2*quiet
	var d strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Dark(x, y) {
				fmt.Fprintf(&d, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code"><path fill="currentColor" d="%s"/></svg>`, total, total, d.String())
}

// block describes the Reed–Solomon structure of one version at level M:
// ecPerBlock EC codewords per block; g1 blocks of g1Data data codewords,
// then g2 blocks of g2Data (g2Data = g1Data + 1 when g2 > 0).
type block struct {
	ecPerBlock, g1, g1Data, g2, g2Data int
}

// levelM is Table 9 of the specification for level M, versions 1–10.
var levelM = [...]block{
	{}, // no version 0
	{10, 1, 16, 0, 0},
	{16, 1, 28, 0, 0},
	{26, 1, 44, 0, 0},
	{18, 2, 32, 0, 0},
	{24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0},
	{18, 4, 31, 0, 0},
	{22, 2, 38, 2, 39},
	{22, 3, 36, 2, 37},
	{26, 4, 43, 1, 44},
}

// alignment lists the alignment pattern centre coordinates per version.
var alignment = [...][]int{
	nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
}

const maxVersion = 10

func (b block) dataCodewords() int { return b.g1*b.g1Data + b.g2*b.g2Data }

// Encode encodes data at level M in the smallest version that fits.
func Encode(data []byte) (*Code, error) {
	version := 0
	for v := 1; v <= maxVersion; v++ {
		if 4+countBits(v)+8*len(data) <= levelM[v].dataCodewords()*8 { // mode, count, data
			version = v
			break
		}
	}
	if version == 0 {
		return nil, ErrTooLong
	}
	codewords := interleave(version, dataCodewords(version, data))
	c := newCanvas(version)
	c.drawFunctionPatterns()
	c.placeData(codewords)
	c.applyBestMask()
	return &Code{Version: version, Size: c.size, modules: c.modules}, nil
}

// dataCodewords is the byte-mode bit stream, terminated and padded to the
// version's data capacity: mode 0100, an 8-bit count, the bytes, up to four
// zero terminator bits, zero bits to a byte boundary, then 0xEC 0x11 …
func dataCodewords(version int, data []byte) []byte {
	capacity := levelM[version].dataCodewords()
	var bits bitWriter
	bits.write(0b0100, 4)
	bits.write(len(data), countBits(version))
	for _, b := range data {
		bits.write(int(b), 8)
	}
	for i := 0; i < 4 && len(bits.bits) < capacity*8; i++ {
		bits.write(0, 1)
	}
	for len(bits.bits)%8 != 0 {
		bits.write(0, 1)
	}
	out := bits.bytes()
	for pad := byte(0xEC); len(out) < capacity; pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

// countBits is the width of the byte-mode character count: 8 bits for
// versions 1–9, 16 from version 10.
func countBits(version int) int {
	if version >= 10 {
		return 16
	}
	return 8
}

type bitWriter struct{ bits []bool }

func (w *bitWriter) write(v, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, (v>>uint(i))&1 == 1)
	}
}

func (w *bitWriter) bytes() []byte {
	out := make([]byte, len(w.bits)/8)
	for i, b := range w.bits {
		if b {
			out[i/8] |= 0x80 >> uint(i%8)
		}
	}
	return out
}

// interleave splits the data codewords into the version's blocks, computes
// each block's EC codewords, and interleaves both as the specification
// orders them: the first codeword of every block, then the second, …,
// followed by the EC codewords the same way.
func interleave(version int, data []byte) []byte {
	b := levelM[version]
	var blocks [][]byte
	pos := 0
	for i := 0; i < b.g1; i++ {
		blocks = append(blocks, data[pos:pos+b.g1Data])
		pos += b.g1Data
	}
	for i := 0; i < b.g2; i++ {
		blocks = append(blocks, data[pos:pos+b.g2Data])
		pos += b.g2Data
	}
	ecs := make([][]byte, len(blocks))
	for i, blk := range blocks {
		ecs[i] = reedSolomon(blk, b.ecPerBlock)
	}
	longest := b.g1Data
	if b.g2 > 0 {
		longest = b.g2Data
	}
	var out []byte
	for i := 0; i < longest; i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	for i := 0; i < b.ecPerBlock; i++ {
		for _, ec := range ecs {
			out = append(out, ec[i])
		}
	}
	return out
}

// GF(256) with the QR polynomial x^8 + x^4 + x^3 + x^2 + 1 (0x11D).
var gfExp, gfLog = func() ([512]byte, [256]byte) {
	var exp [512]byte
	var log [256]byte
	x := 1
	for i := 0; i < 255; i++ {
		exp[i] = byte(x)
		log[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		exp[i] = exp[i-255]
	}
	return exp, log
}()

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// generator is the RS generator polynomial ∏(x − α^i), i = 0..n−1, highest
// degree first.
func generator(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= gfMul(c, gfExp[i])
		}
		g = next
	}
	return g
}

// reedSolomon returns the n EC codewords for data: the remainder of
// data·x^n divided by the generator.
func reedSolomon(data []byte, n int) []byte {
	g := generator(n)
	rem := make([]byte, n)
	for _, d := range data {
		factor := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for i := range rem {
			rem[i] ^= gfMul(g[i+1], factor)
		}
	}
	return rem
}

// canvas is the symbol under construction.
type canvas struct {
	version, size int
	modules       []bool
	function      []bool // modules that carry patterns, not data
}

func newCanvas(version int) *canvas {
	size := 17 + 4*version
	return &canvas{version: version, size: size, modules: make([]bool, size*size), function: make([]bool, size*size)}
}

func (c *canvas) set(x, y int, dark bool) {
	c.modules[y*c.size+x] = dark
	c.function[y*c.size+x] = true
}

func (c *canvas) drawFunctionPatterns() {
	// Timing patterns.
	for i := 0; i < c.size; i++ {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	// Finder patterns with their separators.
	c.drawFinder(3, 3)
	c.drawFinder(c.size-4, 3)
	c.drawFinder(3, c.size-4)
	// Alignment patterns, except where a finder is.
	pos := alignment[c.version]
	for i, cy := range pos {
		for j, cx := range pos {
			corner := (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0)
			if !corner {
				c.drawAlignment(cx, cy)
			}
		}
	}
	// Reserve the format areas (drawn per mask) and the version areas.
	c.drawFormat(0)
	c.drawVersion()
}

func (c *canvas) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= c.size || y >= c.size {
				continue
			}
			dist := max(abs(dx), abs(dy))
			c.set(x, y, dist != 2 && dist != 4)
		}
	}
}

func (c *canvas) drawAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			c.set(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

// formatBits is the 15-bit format information for level M and mask:
// 5 data bits (level 00, mask), BCH(15,5) with generator 0x537, XOR 0x5412.
func formatBits(mask int) int {
	data := mask // level M is 00, so the five bits are the mask alone
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return ((data << 10) | rem) ^ 0x5412
}

// drawFormat writes the format information in both places, and the dark
// module that always sits above the bottom-left finder.
func (c *canvas) drawFormat(mask int) {
	bits := formatBits(mask)
	bit := func(i int) bool { return (bits>>uint(i))&1 == 1 }
	// Around the top-left finder.
	for i := 0; i <= 5; i++ {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	// Split between the other two finders.
	for i := 0; i < 8; i++ {
		c.set(c.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, c.size-15+i, bit(i))
	}
	c.set(8, c.size-8, true)
}

// versionBits is the 18-bit version information (versions 7 and up):
// 6 bits of version, BCH(18,6) with generator 0x1F25.
func versionBits(version int) int {
	rem := version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return (version << 12) | rem
}

func (c *canvas) drawVersion() {
	if c.version < 7 {
		return
	}
	bits := versionBits(c.version)
	for i := 0; i < 18; i++ {
		dark := (bits>>uint(i))&1 == 1
		a, b := c.size-11+i%3, i/3
		c.set(a, b, dark)
		c.set(b, a, dark)
	}
}

// placeData lays the codeword bits into the non-function modules in the
// specification's zigzag: two-module columns from the right, alternately
// upward and downward, skipping the vertical timing column.
func (c *canvas) placeData(codewords []byte) {
	i := 0
	total := len(codewords) * 8
	for right := c.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < c.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := ((right + 1) & 2) == 0
				y := vert
				if upward {
					y = c.size - 1 - vert
				}
				if c.function[y*c.size+x] || i >= total {
					continue
				}
				c.modules[y*c.size+x] = (codewords[i/8]>>uint(7-i%8))&1 == 1
				i++
			}
		}
	}
}

// maskDark says whether mask m inverts the module at column x, row y.
func maskDark(m, x, y int) bool {
	switch m {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func (c *canvas) applyMask(m int) {
	for y := 0; y < c.size; y++ {
		for x := 0; x < c.size; x++ {
			if !c.function[y*c.size+x] && maskDark(m, x, y) {
				c.modules[y*c.size+x] = !c.modules[y*c.size+x]
			}
		}
	}
}

// applyBestMask tries all eight masks and keeps the one with the lowest
// penalty, each scored with its own format information in place.
func (c *canvas) applyBestMask() {
	best, bestScore := 0, int(^uint(0)>>1)
	for m := 0; m < 8; m++ {
		c.applyMask(m)
		c.drawFormat(m)
		if s := c.penalty(); s < bestScore {
			best, bestScore = m, s
		}
		c.applyMask(m) // masks are their own inverse
	}
	c.applyMask(best)
	c.drawFormat(best)
}

// penalty is the sum of the four scoring rules (specification 8.8.2).
func (c *canvas) penalty() int {
	score := 0
	dark := 0
	// Rule 1: runs of five or more same-coloured modules, rows and columns.
	for y := 0; y < c.size; y++ {
		runColour, run := false, 0
		for x := 0; x < c.size; x++ {
			d := c.Dark(x, y)
			if d {
				dark++
			}
			if x > 0 && d == runColour {
				run++
				if run == 5 {
					score += 3
				} else if run > 5 {
					score++
				}
			} else {
				runColour, run = d, 1
			}
		}
	}
	for x := 0; x < c.size; x++ {
		runColour, run := false, 0
		for y := 0; y < c.size; y++ {
			d := c.Dark(x, y)
			if y > 0 && d == runColour {
				run++
				if run == 5 {
					score += 3
				} else if run > 5 {
					score++
				}
			} else {
				runColour, run = d, 1
			}
		}
	}
	// Rule 2: 2×2 blocks of one colour.
	for y := 0; y < c.size-1; y++ {
		for x := 0; x < c.size-1; x++ {
			d := c.Dark(x, y)
			if d == c.Dark(x+1, y) && d == c.Dark(x, y+1) && d == c.Dark(x+1, y+1) {
				score += 3
			}
		}
	}
	// Rule 3: finder-like 1:1:3:1:1 runs with four light modules beside them.
	for y := 0; y < c.size; y++ {
		for x := 0; x+10 < c.size; x++ {
			if c.finderLike(func(i int) bool { return c.Dark(x+i, y) }) {
				score += 40
			}
			if c.finderLike(func(i int) bool { return c.Dark(y, x+i) }) {
				score += 40
			}
		}
	}
	// Rule 4: dark-module proportion, 10 per 5 % step away from 50 %.
	total := c.size * c.size
	deviation := abs(dark*20-total*10) / total // in 5 % steps, rounded down
	score += deviation * 10
	return score
}

// finderLike reports whether the eleven modules at get(0..10) hold the
// pattern light ×4, dark, light, dark ×3, light, dark or its mirror.
func (c *canvas) finderLike(get func(i int) bool) bool {
	pattern := [11]bool{false, false, false, false, true, false, true, true, true, false, true}
	forward, backward := true, true
	for i := 0; i < 11; i++ {
		if get(i) != pattern[i] {
			forward = false
		}
		if get(i) != pattern[10-i] {
			backward = false
		}
	}
	return forward || backward
}

func (c *canvas) Dark(x, y int) bool { return c.modules[y*c.size+x] }

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
