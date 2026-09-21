package qr

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Reed–Solomon codewords of the specification's worked example: the
// 16 data codewords of "HELLO WORLD" at version 1-M and their ten EC
// codewords (as reproduced in every tutorial that walks the standard).
func TestReedSolomonKnownVector(t *testing.T) {
	data := []byte{0x20, 0x5B, 0x0B, 0x78, 0xD1, 0x72, 0xDC, 0x4D, 0x43, 0x40, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11}
	want := []byte{0xC4, 0x23, 0x27, 0x77, 0xEB, 0xD7, 0xE7, 0xE2, 0x5D, 0x17}
	if got := reedSolomon(data, 10); !bytes.Equal(got, want) {
		t.Fatalf("EC codewords\n got  % X\n want % X", got, want)
	}
}

// Independently of any vector: a codeword (data ‖ EC) evaluates to zero at
// every root of the generator — the syndromes a decoder would check.
func TestReedSolomonSyndromesAreZero(t *testing.T) {
	data := []byte("dialler://enrol?c=A7K2M9PX&f=RqsfmlZs_xyf6U-AtYDUUs0nqge6keGw4PqE0xQCHd0&h=10.18.0.212&p=8080")
	for _, n := range []int{10, 16, 18, 22, 24, 26} {
		cw := append(append([]byte{}, data...), reedSolomon(data, n)...)
		for i := 0; i < n; i++ {
			root := gfExp[i]
			var acc byte
			for _, c := range cw { // Horner's rule, highest degree first
				acc = gfMul(acc, root) ^ c
			}
			if acc != 0 {
				t.Fatalf("n=%d: syndrome %d is %#x, want 0", n, i, acc)
			}
		}
	}
}

// Format and version information against the specification's own examples.
func TestFormatAndVersionInformation(t *testing.T) {
	if got := formatBits(0); got != 0x5412 {
		t.Fatalf("format M/mask 0: %#x, want 0x5412", got)
	}
	if got := formatBits(5); got != 0x40CE {
		t.Fatalf("format M/mask 5: %#x, want 0x40CE", got)
	}
	if got := versionBits(7); got != 0x07C94 {
		t.Fatalf("version 7: %#x, want 0x07C94", got)
	}
	if got := versionBits(10); got != 0x0A4D3 {
		t.Fatalf("version 10: %#x, want 0x0A4D3", got)
	}
}

// The function patterns leave exactly the specification's number of data
// modules in every version: total codewords × 8 plus the remainder bits
// (7 for versions 2–6, none for 1 and 7–13). Any misplaced pattern — a
// missing alignment pattern, a version block on the wrong side — changes
// this count.
func TestDataModuleCountPerVersion(t *testing.T) {
	remainder := map[int]int{2: 7, 3: 7, 4: 7, 5: 7, 6: 7}
	for v := 1; v <= maxVersion; v++ {
		c := newCanvas(v)
		c.drawFunctionPatterns()
		free := 0
		for _, f := range c.function {
			if !f {
				free++
			}
		}
		b := levelM[v]
		total := b.dataCodewords() + (b.g1+b.g2)*b.ecPerBlock
		if want := total*8 + remainder[v]; free != want {
			t.Fatalf("version %d: %d data modules, want %d", v, free, want)
		}
	}
}

func TestVersionSelectionAndCapacity(t *testing.T) {
	short, _ := Encode([]byte("hi"))
	if short.Version != 1 || short.Size != 21 {
		t.Fatalf("short: version %d size %d", short.Version, short.Size)
	}
	link, _ := Encode([]byte("dialler://enrol?c=A7K2M9PX&f=RqsfmlZs_xyf6U-AtYDUUs0nqge6keGw4PqE0xQCHd0&h=10.18.0.212&p=8080"))
	if link.Version != 6 || link.Size != 41 {
		t.Fatalf("link (94 bytes): version %d size %d, want 6 / 41", link.Version, link.Size)
	}
	if _, err := Encode(bytes.Repeat([]byte{'x'}, 213)); err != nil {
		t.Fatalf("213 bytes must fit version 10-M: %v", err)
	}
	if _, err := Encode(bytes.Repeat([]byte{'x'}, 214)); err != ErrTooLong {
		t.Fatalf("214 bytes: %v", err)
	}
}

// The three finders, the dark module and the timing patterns are where the
// specification puts them, whatever mask was chosen.
func TestFixedPatterns(t *testing.T) {
	c, _ := Encode([]byte("dialler://enrol?c=A7K2M9PX&h=srv&p=8080"))
	finder := func(cx, cy int) {
		for dy := -3; dy <= 3; dy++ {
			for dx := -3; dx <= 3; dx++ {
				want := max(abs(dx), abs(dy)) != 2
				if c.Dark(cx+dx, cy+dy) != want {
					t.Fatalf("finder at %d,%d wrong at %d,%d", cx, cy, dx, dy)
				}
			}
		}
	}
	finder(3, 3)
	finder(c.Size-4, 3)
	finder(3, c.Size-4)
	if !c.Dark(8, c.Size-8) {
		t.Fatal("dark module missing")
	}
	for i := 8; i < c.Size-8; i++ {
		if c.Dark(i, 6) != (i%2 == 0) || c.Dark(6, i) != (i%2 == 0) {
			t.Fatalf("timing pattern wrong at %d", i)
		}
	}
	if !strings.HasPrefix(c.SVG(), `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `) || !strings.Contains(c.SVG(), `fill="currentColor"`) {
		t.Fatal("svg shape")
	}
}

// Writes a symbol as a PNG when QR_PNG is set (the enrolment link, or
// QR_TEXT), for an independent decode with the system's detector:
//
//	QR_PNG=$TMPDIR/qr.png go test ./internal/qr -run TestWritePNG && swift tools/qr_decode.swift $TMPDIR/qr.png
func TestWritePNG(t *testing.T) {
	path := os.Getenv("QR_PNG")
	if path == "" {
		t.Skip("QR_PNG not set")
	}
	text := os.Getenv("QR_TEXT")
	if text == "" {
		text = "dialler://enrol?c=A7K2M9PX&f=RqsfmlZs_xyf6U-AtYDUUs0nqge6keGw4PqE0xQCHd0&h=10.18.0.212&p=8080"
	}
	c, err := Encode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	const scale, quiet = 8, 4
	px := (c.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, px, px))
	for y := 0; y < px; y++ {
		for x := 0; x < px; x++ {
			mx, my := x/scale-quiet, y/scale-quiet
			col := color.Gray{Y: 255}
			if mx >= 0 && my >= 0 && mx < c.Size && my < c.Size && c.Dark(mx, my) {
				col = color.Gray{Y: 0}
			}
			img.SetGray(x, y, col)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "qr.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
