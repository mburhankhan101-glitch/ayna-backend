package photo

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

// Composite multiplies a white-ground overlay onto the photo it describes.
//
// AILab's `red_area` map is an opaque JPEG: white everywhere it has nothing to
// say, tinted where it does. Multiply is exactly the right blend for that --
// white is the identity, so untouched areas show the photo through unchanged
// while tinted areas darken toward the tint. Drawing it with alpha instead
// would wash the whole face out with a white veil.
//
// **Only the composited result is stored.** The alternative -- keeping the
// original photo and blending on the client -- would mean holding a clean face
// photograph of every user for every scan, which is a materially heavier thing
// to be responsible for than an overlay that is already marked up. It also
// halves the bytes.
//
// Returns nil when either image is unusable or their sizes disagree. A missing
// overlay renders as a report without one, which is a tier the product already
// has; a wrong overlay is a claim about someone's face.
func Composite(base, overlay []byte) []byte {
	out, err := composite(base, overlay)
	if err != nil {
		return nil
	}
	return out
}

func composite(base, overlay []byte) ([]byte, error) {
	b, err := jpeg.Decode(bytes.NewReader(base))
	if err != nil {
		return nil, fmt.Errorf("decode base: %w", err)
	}
	o, err := jpeg.Decode(bytes.NewReader(overlay))
	if err != nil {
		return nil, fmt.Errorf("decode overlay: %w", err)
	}

	bb, ob := b.Bounds(), o.Bounds()
	if bb.Dx() != ob.Dx() || bb.Dy() != ob.Dy() {
		// The vendor returns maps at the dimensions it was given, so a mismatch
		// means the two came from different requests. Blending them would put
		// one person's findings on another person's face.
		return nil, fmt.Errorf(
			"size mismatch: base %dx%d, overlay %dx%d",
			bb.Dx(), bb.Dy(), ob.Dx(), ob.Dy(),
		)
	}

	dst := image.NewRGBA(bb)
	draw.Draw(dst, bb, b, bb.Min, draw.Src)

	for y := bb.Min.Y; y < bb.Max.Y; y++ {
		for x := bb.Min.X; x < bb.Max.X; x++ {
			br, bg, bbl, _ := b.At(x, y).RGBA()
			or, og, obl, _ := o.At(x, y).RGBA()

			dst.Set(x, y, color.RGBA{
				mul(br, or), mul(bg, og), mul(bbl, obl), 255,
			})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), nil
}

// mul multiplies two 16-bit channel values down to 8 bits.
//
// Multiply rather than a weighted average because white must be the identity:
// the overlay is mostly white, and any blend that darkened those pixels would
// grey out the entire photograph.
func mul(a, b uint32) uint8 {
	return uint8((a >> 8) * (b >> 8) / 255)
}
