// Package qr is a small QR code encoder (ISO/IEC 18004) for rendering
// enrolment links offline: byte mode, error correction level M, versions
// 1–10. It is deliberately narrow — the input is a short ASCII URL — but
// what is in scope follows the standard exactly: Reed–Solomon over
// GF(256) with the QR primitive polynomial, the level-M block structure
// with interleaving, all function patterns, format and version
// information, and the eight masks scored by the four penalty rules.
package qr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MaxBytes is the largest input Encode accepts: the byte-mode capacity of
// version 10 at level M.
const MaxBytes = 213

// ErrTooLong is returned by Encode when the input does not fit version 10.
var ErrTooLong = errors.New("qr: input exceeds 213 bytes")

// Code is an encoded symbol: the module matrix without the quiet zone.
type Code struct {
	Version int // symbol version, 1–10
	Size    int // modules per side, 17 + 4*Version
	Mask    int // data mask pattern chosen, 0–7
	modules [][]bool
}

// At reports whether the module at column x, row y is dark. Coordinates
// outside the symbol are light.
func (c *Code) At(x, y int) bool {
	if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
		return false
	}
	return c.modules[y][x]
}

// quietZone is the light border the SVG draws around the symbol, in modules.
const quietZone = 4

// SVG renders the code with a four-module quiet zone as an inline SVG
// string. The viewBox is in module units and no width/height is set, so
// CSS sizes it. All dark modules are one <path>; each row's runs of dark
// modules are one subpath. The output is deterministic.
func (c *Code) SVG() string {
	n := c.Size + 2*quietZone
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(n))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" shape-rendering="crispEdges" role="img"><rect width="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" fill="#fff"/><path fill="#000" d="`)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; {
			if !c.modules[y][x] {
				x++
				continue
			}
			run := 0
			for x+run < c.Size && c.modules[y][x+run] {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+quietZone, y+quietZone, run, run)
			x += run
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

// Standard tables for level M, versions 1–10 (ISO/IEC 18004 Table 9 and
// Annex E). Index 0 is unused.

// ecBlocks gives the number of error correction codewords per block and
// the number of blocks. Where the total codewords do not divide evenly the
// last (total mod blocks) blocks carry one extra data codeword; the block
// lengths in the standard (e.g. v8-M: 2×(60,38) + 2×(61,39)) follow.
var ecBlocks = [11]struct{ ecPerBlock, blocks int }{
	{}, {10, 1}, {16, 1}, {26, 1}, {18, 2}, {24, 2}, {16, 4}, {18, 4}, {22, 4}, {22, 5}, {26, 5},
}

// alignPositions are the row/column centre coordinates of the alignment
// patterns; every combination is placed except the three finder corners.
var alignPositions = [11][]int{
	nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
}

// byteCapacity is the byte-mode data capacity at level M.
var byteCapacity = [11]int{0, 14, 26, 42, 62, 84, 106, 122, 152, 180, 213}

// Encode returns the module matrix (true = dark), without the quiet zone,
// for data in byte mode at error correction level M, using the smallest
// version 1–10 that fits. Inputs over MaxBytes return ErrTooLong.
func Encode(data []byte) (*Code, error) {
	version := 0
	for v := 1; v <= 10; v++ {
		if len(data) <= byteCapacity[v] {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, ErrTooLong
	}

	g := newGrid(version)
	g.drawFunctionPatterns()

	total := g.rawDataModules() / 8
	dataCodewords := total - ecBlocks[version].ecPerBlock*ecBlocks[version].blocks
	codewords := interleave(version, total, encodeBytes(version, data, dataCodewords))
	g.placeCodewords(codewords)

	best, bestScore := 0, -1
	for m := 0; m < 8; m++ {
		g.applyMask(m)
		g.drawFormatBits(m)
		if s := g.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = m, s
		}
		g.applyMask(m) // XOR is its own inverse
	}
	g.applyMask(best)
	g.drawFormatBits(best)

	return &Code{Version: version, Size: g.size, Mask: best, modules: g.modules}, nil
}

// bit stream

type bitBuf struct {
	bits []bool
}

func (b *bitBuf) append(val uint, n int) {
	for i := n - 1; i >= 0; i-- {
		b.bits = append(b.bits, (val>>uint(i))&1 == 1)
	}
}

// encodeBytes builds the byte-mode segment for data and pads it to
// dataCodewords bytes: mode indicator, character count, data, terminator,
// alignment to a byte boundary, then 0xEC/0x11 pad codewords.
func encodeBytes(version int, data []byte, dataCodewords int) []byte {
	var b bitBuf
	b.append(0b0100, 4)
	if version <= 9 {
		b.append(uint(len(data)), 8)
	} else {
		b.append(uint(len(data)), 16)
	}
	for _, c := range data {
		b.append(uint(c), 8)
	}
	capBits := dataCodewords * 8
	if len(b.bits) > capBits {
		panic("qr: segment overflows capacity")
	}
	term := 4
	if rem := capBits - len(b.bits); rem < term {
		term = rem
	}
	b.append(0, term)
	if pad := (8 - len(b.bits)%8) % 8; pad > 0 {
		b.append(0, pad)
	}
	for pad := byte(0xEC); len(b.bits) < capBits; pad ^= 0xEC ^ 0x11 {
		b.append(uint(pad), 8)
	}
	out := make([]byte, dataCodewords)
	for i, bit := range b.bits {
		if bit {
			out[i/8] |= 0x80 >> uint(i%8)
		}
	}
	return out
}

// interleave splits the data codewords into the level-M blocks for this
// version, appends each block's Reed–Solomon codewords, and interleaves
// the blocks as the standard requires: all blocks' data codewords column
// by column (short blocks skip their missing final column), then all
// blocks' EC codewords column by column.
func interleave(version, total int, data []byte) []byte {
	ecLen, numBlocks := ecBlocks[version].ecPerBlock, ecBlocks[version].blocks
	shortLen := total / numBlocks
	numShort := numBlocks - total%numBlocks
	shortData := shortLen - ecLen

	gen := rsGenerator(ecLen)
	blocks := make([][]byte, numBlocks)
	k := 0
	for i := range blocks {
		n := shortData
		if i >= numShort {
			n++
		}
		blocks[i] = data[k : k+n]
		k += n
	}
	if k != len(data) {
		panic("qr: block sizes do not sum to data length")
	}
	ecs := make([][]byte, numBlocks)
	for i, blk := range blocks {
		ecs[i] = rsRemainder(blk, gen)
	}

	out := make([]byte, 0, total)
	for col := 0; col <= shortData; col++ {
		for _, blk := range blocks {
			if col < len(blk) {
				out = append(out, blk[col])
			}
		}
	}
	for col := 0; col < ecLen; col++ {
		for _, ec := range ecs {
			out = append(out, ec[col])
		}
	}
	return out
}

// GF(256) with the QR primitive polynomial x^8 + x^4 + x^3 + x^2 + 1 (0x11D).

var gfExp, gfLog = func() (exp [512]byte, log [256]byte) {
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
	return
}()

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// rsGenerator returns the generator polynomial of the given degree,
// ∏(x − α^i) for i = 0..degree−1, as its coefficients from x^(degree−1)
// down to x^0 (the leading x^degree coefficient of 1 is implicit).
func rsGenerator(degree int) []byte {
	g := make([]byte, degree)
	g[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range g {
			g[j] = gfMul(g[j], root)
			if j+1 < len(g) {
				g[j] ^= g[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return g
}

// rsRemainder returns the error correction codewords for data: the
// remainder of data·x^degree divided by the generator polynomial.
func rsRemainder(data, gen []byte) []byte {
	r := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i, c := range gen {
			r[i] ^= gfMul(c, factor)
		}
	}
	return r
}

// formatBits returns the 15-bit format information for level M and the
// given mask: 5 data bits (00 for M, then the mask), the BCH(15,5)
// remainder with generator x^10+x^8+x^5+x^4+x^2+x+1 (0x537), all XORed
// with the mask pattern 0x5412.
func formatBits(mask int) uint {
	data := uint(mask) // level M is 00, so the data bits are just the mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

// versionBits returns the 18-bit version information for version 7 and
// above: 6 version bits and the BCH(18,6) remainder with generator
// x^12+x^11+x^10+x^9+x^8+x^5+x^2+1 (0x1F25).
func versionBits(version int) uint {
	rem := uint(version)
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return uint(version)<<12 | rem
}

// grid

type grid struct {
	version  int
	size     int
	modules  [][]bool // true = dark
	function [][]bool // true = function pattern (or format/version info)
}

func newGrid(version int) *grid {
	size := 17 + 4*version
	g := &grid{version: version, size: size}
	g.modules = make([][]bool, size)
	g.function = make([][]bool, size)
	for i := range g.modules {
		g.modules[i] = make([]bool, size)
		g.function[i] = make([]bool, size)
	}
	return g
}

func (g *grid) setFunction(x, y int, dark bool) {
	g.modules[y][x] = dark
	g.function[y][x] = true
}

func (g *grid) drawFunctionPatterns() {
	// Timing patterns: alternating, dark at even indices.
	for i := 0; i < g.size; i++ {
		g.setFunction(6, i, i%2 == 0)
		g.setFunction(i, 6, i%2 == 0)
	}
	// Finder patterns with separators at three corners.
	g.drawFinder(3, 3)
	g.drawFinder(g.size-4, 3)
	g.drawFinder(3, g.size-4)
	// Alignment patterns, skipping the three finder corners.
	pos := alignPositions[g.version]
	for i, cy := range pos {
		for j, cx := range pos {
			corner := (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0)
			if !corner {
				g.drawAlignment(cx, cy)
			}
		}
	}
	// Reserve the format areas and place the dark module; the bits
	// themselves are drawn once the mask is chosen.
	g.drawFormatBits(0)
	g.drawVersionBits()
}

// drawFinder draws the 7×7 finder pattern centred at (cx, cy) with its
// one-module light separator; modules off the symbol are skipped.
func (g *grid) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= g.size || y >= g.size {
				continue
			}
			d := max(abs(dx), abs(dy))
			g.setFunction(x, y, d != 2 && d != 4)
		}
	}
}

// drawAlignment draws the 5×5 alignment pattern centred at (cx, cy).
func (g *grid) drawAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			g.setFunction(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

// drawFormatBits writes the format information in both places and the
// dark module. Bit 14 (MSB) is written first along the top-left finder
// reading down the column and then along the row.
func (g *grid) drawFormatBits(mask int) {
	bits := formatBits(mask)
	bit := func(i int) bool { return (bits>>uint(i))&1 == 1 }
	// First copy, around the top-left finder.
	for i := 0; i <= 5; i++ {
		g.setFunction(8, i, bit(i))
	}
	g.setFunction(8, 7, bit(6))
	g.setFunction(8, 8, bit(7))
	g.setFunction(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		g.setFunction(14-i, 8, bit(i))
	}
	// Second copy, split between the other two finders.
	for i := 0; i < 8; i++ {
		g.setFunction(g.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		g.setFunction(8, g.size-15+i, bit(i))
	}
	// The dark module.
	g.setFunction(8, g.size-8, true)
}

// drawVersionBits writes the version information (versions 7+) in the two
// 6×3 blocks beside the top-right and bottom-left finders.
func (g *grid) drawVersionBits() {
	if g.version < 7 {
		return
	}
	bits := versionBits(g.version)
	for i := 0; i < 18; i++ {
		bit := (bits>>uint(i))&1 == 1
		a := g.size - 11 + i%3
		b := i / 3
		g.setFunction(a, b, bit)
		g.setFunction(b, a, bit)
	}
}

// rawDataModules counts the modules not used by function patterns, format
// or version information: 8 × the total codewords plus the remainder bits.
func (g *grid) rawDataModules() int {
	n := 0
	for y := range g.function {
		for x := range g.function[y] {
			if !g.function[y][x] {
				n++
			}
		}
	}
	return n
}

// placeCodewords writes the codeword bits, MSB first, in the standard
// zigzag: two-module-wide columns from the right edge, alternately upward
// and downward, skipping the vertical timing column. Remainder bits are
// left light.
func (g *grid) placeCodewords(cw []byte) {
	i := 0
	for right := g.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		upward := ((right + 1) & 2) == 0
		for vert := 0; vert < g.size; vert++ {
			y := vert
			if upward {
				y = g.size - 1 - vert
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if g.function[y][x] || i >= len(cw)*8 {
					continue
				}
				g.modules[y][x] = (cw[i>>3]>>uint(7-i&7))&1 == 1
				i++
			}
		}
	}
	if i != len(cw)*8 {
		panic("qr: codewords do not fit the symbol")
	}
}

// maskBit reports whether mask pattern m inverts the module at column x, row y.
func maskBit(m, x, y int) bool {
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
	case 7:
		return ((x+y)%2+x*y%3)%2 == 0
	}
	panic("qr: bad mask")
}

// applyMask XORs mask pattern m over the non-function modules.
func (g *grid) applyMask(m int) {
	for y := 0; y < g.size; y++ {
		for x := 0; x < g.size; x++ {
			if !g.function[y][x] && maskBit(m, x, y) {
				g.modules[y][x] = !g.modules[y][x]
			}
		}
	}
}

// Penalty weights from the standard.
const (
	penaltyN1 = 3  // adjacent modules of the same colour in a row/column, run ≥ 5
	penaltyN2 = 3  // each 2×2 block of the same colour
	penaltyN3 = 40 // 1:1:3:1:1 finder-like pattern with 4 light modules beside it
	penaltyN4 = 10 // per 5% deviation of the dark proportion from 50%
)

// penalty scores the current symbol with the four mask evaluation rules.
func (g *grid) penalty() int {
	score := 0
	line := make([]bool, g.size)
	for k := 0; k < g.size; k++ {
		// Row k, then column k.
		for pass := 0; pass < 2; pass++ {
			for i := 0; i < g.size; i++ {
				if pass == 0 {
					line[i] = g.modules[k][i]
				} else {
					line[i] = g.modules[i][k]
				}
			}
			score += penaltyRuns(line) + penaltyFinderLike(line)
		}
	}
	dark := 0
	for y := 0; y < g.size; y++ {
		for x := 0; x < g.size; x++ {
			if g.modules[y][x] {
				dark++
			}
			if x+1 < g.size && y+1 < g.size {
				c := g.modules[y][x]
				if g.modules[y][x+1] == c && g.modules[y+1][x] == c && g.modules[y+1][x+1] == c {
					score += penaltyN2
				}
			}
		}
	}
	total := g.size * g.size
	// k = ceil(|dark% − 50| / 5) − 1, computed in integers as
	// ceil(|20·dark − 10·total| / total) − 1.
	dev := abs(20*dark - 10*total)
	k := (dev+total-1)/total - 1
	score += k * penaltyN4
	return score
}

// penaltyRuns applies rule 1 to one row or column.
func penaltyRuns(line []bool) int {
	score := 0
	for i := 0; i < len(line); {
		run := 1
		for i+run < len(line) && line[i+run] == line[i] {
			run++
		}
		if run >= 5 {
			score += penaltyN1 + run - 5
		}
		i += run
	}
	return score
}

// penaltyFinderLike applies rule 3 to one row or column: each occurrence
// of dark-light-dark-dark-dark-light-dark preceded by four light modules,
// and each occurrence followed by four light modules, scores N3.
func penaltyFinderLike(line []bool) int {
	const pattern = 0b1011101
	score := 0
	for i := 0; i+7 <= len(line); i++ {
		v := 0
		for j := 0; j < 7; j++ {
			v <<= 1
			if line[i+j] {
				v |= 1
			}
		}
		if v != pattern {
			continue
		}
		if i >= 4 && !line[i-1] && !line[i-2] && !line[i-3] && !line[i-4] {
			score += penaltyN3
		}
		if i+11 <= len(line) && !line[i+7] && !line[i+8] && !line[i+9] && !line[i+10] {
			score += penaltyN3
		}
	}
	return score
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
