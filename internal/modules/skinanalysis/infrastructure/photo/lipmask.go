package photo

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
)

// Rect is a bounding box as the vendor reports one.
type Rect struct {
	Left, Top, Width, Height int
}

func (r Rect) center() (float64, float64) {
	return float64(r.Left) + float64(r.Width)/2,
		float64(r.Top) + float64(r.Height)/2
}

func (r Rect) valid() bool { return r.Width > 0 && r.Height > 0 }

// Mouth position and size, in interocular distances so they scale with the face
// rather than the image.
//
// Deliberately generous. Hiding a little genuine chin redness costs almost
// nothing; leaving the lips lit costs the credibility of the whole overlay.
//
// Tuned against a single face, and a three-quarter one at that. Treat as
// provisional until seen on a spread of real captures.
const (
	mouthDrop   = 1.20
	mouthWidth  = 0.55
	mouthHeight = 0.38
)

// MaskMouth blanks the mouth out of a redness overlay.
//
// Lips are the reddest thing on a face and are supposed to be. AILab's
// `red_area` map dutifully marks them as the most intense region on it, so
// showing the map unedited tells a user their worst redness is their mouth --
// wrong, and slightly insulting, since it is the one red patch nobody was ever
// going to treat.
//
// There is no mouth rectangle in the response, so the mouth is derived from the
// two eye rectangles that are there (nested under `dark_circle_mark`). The eye
// line carries position, scale and roll at once: the mouth sits about 1.2
// interocular distances below its midpoint, along the perpendicular. A tilted
// head is handled for free.
//
// **Yaw is not handled.** A face turned away puts the mouth off that
// perpendicular and the mask drifts, which is the other reason the ellipse is
// oversized.
//
// Returns the image unchanged when the eyes are missing or the image will not
// decode. A cosmetic correction must never be able to destroy the thing it was
// correcting.
func MaskMouth(jpegBytes []byte, leftEye, rightEye Rect) []byte {
	out, err := maskMouth(jpegBytes, leftEye, rightEye)
	if err != nil {
		return jpegBytes
	}
	return out
}

func maskMouth(src []byte, leftEye, rightEye Rect) ([]byte, error) {
	if !leftEye.valid() || !rightEye.valid() {
		return nil, fmt.Errorf("both eye rects are required to locate the mouth")
	}

	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	lx, ly := leftEye.center()
	rx, ry := rightEye.center()

	dx, dy := rx-lx, ry-ly
	d := math.Hypot(dx, dy)
	if d < 1 {
		return nil, fmt.Errorf("eyes coincident; no scale to work from")
	}

	// "Down the face" is perpendicular to the eye line and rotates with the
	// head. Using the image's own vertical would slide the mask off the mouth
	// the moment anyone tilts.
	px, py := -dy/d, dx/d
	if py < 0 {
		px, py = -px, -py
	}

	mx := (lx+rx)/2 + px*mouthDrop*d
	my := (ly+ry)/2 + py*mouthDrop*d

	dst := image.NewRGBA(img.Bounds())
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Src)

	// White, because the map is drawn on white and white is what the client
	// composites as "nothing here". Black would render as a bruise.
	fillEllipse(dst, mx, my, mouthWidth*d, mouthHeight*d)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), nil
}

func fillEllipse(img *image.RGBA, cx, cy, rx, ry float64) {
	b := img.Bounds()
	for y := int(cy - ry); y <= int(cy+ry); y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		for x := int(cx - rx); x <= int(cx+rx); x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			nx := (float64(x) - cx) / rx
			ny := (float64(y) - cy) / ry
			if nx*nx+ny*ny <= 1 {
				img.Set(x, y, color.White)
			}
		}
	}
}
