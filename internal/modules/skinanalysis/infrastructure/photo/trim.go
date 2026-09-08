// Package photo normalises an uploaded selfie before it reaches a vendor.
//
// It does exactly one thing and deliberately not the obvious one: it trims a
// too-tall frame down toward a portrait aspect ratio. It does NOT find the
// face.
//
// Cropping to where the face was *asked* to be is not the same as cropping to
// where it is. Faces are round, long and wide; people sit back, lean and tilt.
// A crop that assumes compliance takes its pixels from the edges of the frame,
// and the edges of a face are the jaw, cheeks and hairline — which is where
// acne lives. It would fail silently: the vendor would analyse a partial face
// and return a confident number about it.
//
// So this only removes what a phone selfie reliably wastes — ceiling above and
// torso below — and never narrows the frame. Anything more precise needs a real
// face detector, and that is a dependency worth adding only once there is
// measured reason to.
package photo

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

// MaxAspect is the tallest height:width ratio kept without trimming.
//
// 4:3. A modern phone shoots 16:9 or taller, which on a selfie is mostly room:
// the frame that measured 826x1280 carried the face across just 4% of its
// pixels, and that is the case the vendor scored as "Clear" when a tighter
// version of the same photo scored "Mild".
const MaxAspect = 4.0 / 3.0

// Anchor is where the kept band's centre sits in the original height.
//
// Slightly above the middle, because people put their head above centre and
// their body below it. Trimming symmetrically would take as much sky as torso
// and remove less of what is actually wasted.
const Anchor = 0.45

// TrimTall crops a too-tall JPEG toward [MaxAspect], keeping the full width.
//
// Width is never touched. That is the safety property: whatever the shape of
// the face, and wherever in the frame it sits horizontally, nothing at the
// sides is lost.
//
// Returns the original bytes unchanged when the image is already within the
// ratio, when it cannot be decoded, or when the arithmetic would produce
// something degenerate. **A normalisation step must never be able to fail a
// scan** — the user has already paid for the photograph with their time, and a
// slightly wasteful frame analyses fine while no frame at all does not.
func TrimTall(src []byte) []byte {
	out, err := trim(src)
	if err != nil {
		return src
	}
	return out
}

func trim(src []byte) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("degenerate bounds")
	}

	keep := int(float64(w) * MaxAspect)
	if keep >= h {
		return nil, fmt.Errorf("already within ratio")
	}

	// Centre the kept band on the anchor, then slide it back inside the image
	// rather than clamping the edges independently — clamping would shrink the
	// band and quietly change the output ratio.
	top := int(float64(h)*Anchor) - keep/2
	if top < 0 {
		top = 0
	}
	if top+keep > h {
		top = h - keep
	}

	sub, ok := img.(interface {
		SubImage(r image.Rectangle) image.Image
	})
	if !ok {
		return nil, fmt.Errorf("image type does not support cropping")
	}

	var buf bytes.Buffer
	// Quality 92: high enough that re-encoding cannot plausibly change a
	// severity score, which would make this step a confound in every future
	// measurement rather than a neutral one.
	err = jpeg.Encode(
		&buf,
		sub.SubImage(image.Rect(b.Min.X, b.Min.Y+top, b.Max.X, b.Min.Y+top+keep)),
		&jpeg.Options{Quality: 92},
	)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	// Only worth it if it actually saved something. A re-encode that grows the
	// file has cost the user upload time for nothing.
	if buf.Len() >= len(src) {
		return nil, fmt.Errorf("re-encode did not shrink the image")
	}

	return buf.Bytes(), nil
}
