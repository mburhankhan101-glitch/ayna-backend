package photo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// redFace builds a stand-in for AILab's red_area map: white ground with a
// saturated red blob where the mouth would be.
func redFace(t *testing.T, mouthX, mouthY int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 513, 513))
	for y := 0; y < 513; y++ {
		for x := 0; x < 513; x++ {
			img.Set(x, y, color.White)
		}
	}
	for y := mouthY - 12; y <= mouthY+12; y++ {
		for x := mouthX - 24; x <= mouthX+24; x++ {
			img.Set(x, y, color.RGBA{220, 30, 40, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func rednessAt(t *testing.T, b []byte, x, y int) int {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, g, bl, _ := img.At(x, y).RGBA()
	// How far this pixel leans red. White scores ~0; a red blob scores high.
	return int(r>>8) - (int(g>>8)+int(bl>>8))/2
}

// Eye rects as AILab reports them, positioned so the derived mouth lands on the
// blob the fixture paints.
var (
	leftEye  = Rect{Left: 166, Top: 179, Width: 39, Height: 33}
	rightEye = Rect{Left: 236, Top: 171, Width: 60, Height: 51}
)

func TestMouthIsBlankedOut(t *testing.T) {
	// Derived from those eyes: interocular ~81px, mouth ~1.2 below the midpoint.
	const mx, my = 225, 293

	src := redFace(t, mx, my)
	if before := rednessAt(t, src, mx, my); before < 100 {
		t.Fatalf("fixture is not red at the mouth (%d)", before)
	}

	out := MaskMouth(src, leftEye, rightEye)
	if after := rednessAt(t, out, mx, my); after > 30 {
		t.Errorf("mouth still reads red after masking (%d) — lips would show "+
			"as the user's worst redness", after)
	}
}

// The mask must not eat the cheeks, which is where the finding actually is.
func TestCheeksSurviveTheMask(t *testing.T) {
	const cheekX, cheekY = 300, 250

	src := redFace(t, cheekX, cheekY) // paint the blob on the cheek instead
	out := MaskMouth(src, leftEye, rightEye)

	if after := rednessAt(t, out, cheekX, cheekY); after < 100 {
		t.Errorf("cheek redness was masked away (%d) — the overlay would hide "+
			"the thing it exists to show", after)
	}
}

// A cosmetic correction must never be able to destroy what it was correcting.
func TestUnusableInputIsReturnedUnchanged(t *testing.T) {
	src := redFace(t, 225, 293)

	for name, tc := range map[string]struct{ l, r Rect }{
		"no left eye":  {Rect{}, rightEye},
		"no right eye": {leftEye, Rect{}},
		"neither":      {Rect{}, Rect{}},
	} {
		if got := MaskMouth(src, tc.l, tc.r); !bytes.Equal(got, src) {
			t.Errorf("%s: image was modified; it must pass through untouched", name)
		}
	}

	if got := MaskMouth([]byte("not an image"), leftEye, rightEye); string(got) != "not an image" {
		t.Error("undecodable input was modified")
	}
}
