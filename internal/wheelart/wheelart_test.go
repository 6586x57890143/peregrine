package wheelart

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/6586x57890143/peregrine/internal/wheel"
)

// The card links to the committed files, so they have to be what this package draws. Pixels
// rather than bytes, because the PNG encoder's compression is free to change between Go
// releases and a byte comparison would fail on an upgrade that changed no picture.
func TestCommittedAssetsMatchTheGenerator(t *testing.T) {
	dir := filepath.Join("..", "..", "assets", "wheel")
	want := All()
	if len(want) != len(wheel.Wedges())+1 {
		t.Fatalf("%d images for %d wedges", len(want), len(wheel.Wedges()))
	}
	for name, img := range want {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s is missing; run go run ./tools/wheelart: %v", name, err)
		}
		got, err := png.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !samePixels(got, img) {
			t.Errorf("%s does not match the generator; run go run ./tools/wheelart", name)
		}
	}
}

func samePixels(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				return false
			}
		}
	}
	return true
}

// Wide enough to set the card's width, and a lit strip differs from the plain one exactly in
// the pointer above its wedge and in every other wedge being dimmed.
func TestStripIsWideAndHighlightsOneWedge(t *testing.T) {
	plain, lit := Strip(-1), Strip(7)
	if b := plain.Bounds(); b.Dx() != Width || b.Dy() != Height || Width < 4*Height {
		t.Fatalf("bounds %v", b)
	}
	n := len(wheel.Wedges())
	mid := func(i int) int { return (i*Width/n + (i+1)*Width/n) / 2 }
	if _, _, _, a := plain.At(mid(7), 1).RGBA(); a != 0 {
		t.Error("the plain strip has a pointer")
	}
	if _, _, _, a := lit.At(mid(7), 1).RGBA(); a == 0 {
		t.Error("the lit wedge has no pointer")
	}
	if _, _, _, a := lit.At(mid(3), 1).RGBA(); a != 0 {
		t.Error("an unlit wedge has a pointer")
	}
	y := Height / 2
	if plain.At(mid(3), y) == lit.At(mid(3), y) {
		t.Error("an unlit wedge is not dimmed")
	}
	if plain.At(mid(7), y) != lit.At(mid(7), y) {
		t.Error("the lit wedge changed colour")
	}
}
