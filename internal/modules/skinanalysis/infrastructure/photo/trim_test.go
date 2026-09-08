package photo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegOf builds a test image with a distinctive band so a crop that takes the
// wrong region is visible rather than merely differently sized.
func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 251), uint8(y % 241), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func boundsOf(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestTrimsATallFrameTowardFourThree(t *testing.T) {
	// The shape a phone actually produces.
	src := jpegOf(t, 1080, 1920)
	out := TrimTall(src)

	w, h := boundsOf(t, out)
	if w != 1080 {
		t.Errorf("width = %d, want 1080 — trimming must never narrow the frame", w)
	}
	want := int(1080 * MaxAspect)
	if h != want {
		t.Errorf("height = %d, want %d", h, want)
	}
}

// The safety property this whole approach rests on.
//
// Width is never touched, whatever the input. That is what makes a blind crop
// acceptable at all: a face can be any shape and sit anywhere horizontally, and
// nothing at the sides — jaw, cheeks, hairline — can be lost.
func TestNeverNarrowsTheFrame(t *testing.T) {
	for _, size := range [][2]int{
		{1080, 1920}, {826, 1280}, {480, 640}, {720, 720}, {1200, 900},
	} {
		src := jpegOf(t, size[0], size[1])
		w, _ := boundsOf(t, TrimTall(src))
		if w != size[0] {
			t.Errorf("%dx%d: width became %d", size[0], size[1], w)
		}
	}
}

func TestLeavesAcceptableRatiosAlone(t *testing.T) {
	for _, size := range [][2]int{
		{720, 720},  // square
		{1200, 900}, // landscape
		{900, 1200}, // exactly 4:3
	} {
		src := jpegOf(t, size[0], size[1])
		if got := TrimTall(src); !bytes.Equal(got, src) {
			t.Errorf("%dx%d was re-encoded when it should have been untouched",
				size[0], size[1])
		}
	}
}

// A normalisation step must never be able to fail a scan.
//
// The user has already spent the effort of taking the photograph, and by this
// point the request is on its way to a paid vendor. A slightly wasteful frame
// analyses perfectly well; no frame at all does not.
func TestGarbageInputIsReturnedUnchanged(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":     {},
		"not jpeg":  []byte("this is not an image"),
		"truncated": jpegOf(t, 1080, 1920)[:40],
	} {
		got := TrimTall(in)
		if !bytes.Equal(got, in) {
			t.Errorf("%s: input was modified; it must pass through untouched", name)
		}
	}
}

// The kept band sits slightly above centre, because heads sit above centre and
// bodies below. A symmetric trim would take as much ceiling as torso and remove
// less of what is actually wasted.
func TestKeptBandFavoursTheUpperHalf(t *testing.T) {
	// Variables, not constants: as constants the expression below stays a
	// constant expression and int() rejects the fraction at compile time.
	w, h := 1000, 2000
	keep := int(float64(w) * MaxAspect)
	top := int(float64(h)*Anchor) - keep/2

	if top >= (h-keep)/2 {
		t.Errorf("band top = %d, expected above the symmetric position %d",
			top, (h-keep)/2)
	}
	if top < 0 || top+keep > h {
		t.Errorf("band [%d, %d) falls outside the image", top, top+keep)
	}
}
