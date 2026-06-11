package overlay

import (
	"bytes"
	"fmt"
	"image"
	"os"
	"regexp"
	"strconv"

	"github.com/jezek/xgb/xproto"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

var reRGBA    = regexp.MustCompile(`rgba\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*[\d.]+\s*\)`)
var reVBParse = regexp.MustCompile(`viewBox="\S*\s+\S*\s+(\S+)\s+(\S+)"`)
var reVB      = regexp.MustCompile(`viewBox="[^"]*"`)
var reSVGTag  = regexp.MustCompile(`<svg\b[^>]*>`)
var reNumber  = regexp.MustCompile(`-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)

// patchSVGColors rewrites rgba() color functions to #RRGGBB hex so that oksvg
// can parse them (it does not recognise CSS rgba() syntax).
func patchSVGColors(data []byte) []byte {
	return reRGBA.ReplaceAllFunc(data, func(match []byte) []byte {
		parts := reRGBA.FindSubmatch(match)
		r, _ := strconv.Atoi(string(parts[1]))
		g, _ := strconv.Atoi(string(parts[2]))
		b, _ := strconv.Atoi(string(parts[3]))
		return []byte(fmt.Sprintf("#%02X%02X%02X", r, g, b))
	})
}

// bakeScale multiplies every number found in val by sx.
func bakeScale(val string, sx float64) string {
	return reNumber.ReplaceAllStringFunc(val, func(s string) string {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return s
		}
		return strconv.FormatFloat(v*sx, 'f', -1, 64)
	})
}

// bakeCoords rewrites the SVG so all coordinate values are expressed in the
// target size's pixel space. This avoids relying on oksvg/rasterx viewport
// scaling for stroke-width, which does not scale correctly with large viewBoxes.
//
// Specifically it:
//   - updates the viewBox and the <svg> tag's own width/height to size×size
//   - scales all numbers in path d= attributes
//   - scales coordinate attributes: cx, cy, r, rx, ry, x1, x2, y1, y2
//   - scales stroke-width so it renders at the intended physical thickness
func bakeCoords(data []byte, size int) []byte {
	m := reVBParse.FindSubmatch(data)
	if m == nil {
		return data
	}
	vbW, e1 := strconv.ParseFloat(string(m[1]), 64)
	vbH, e2 := strconv.ParseFloat(string(m[2]), 64)
	if e1 != nil || e2 != nil || vbW <= 0 || vbH <= 0 {
		return data
	}
	sx := float64(size) / vbW

	// Update viewBox.
	data = reVB.ReplaceAll(data, []byte(fmt.Sprintf(`viewBox="0 0 %d %d"`, size, size)))

	// Update width/height only in the <svg> opening tag (space-prefixed match
	// avoids touching stroke-width or other hyphenated attributes).
	if loc := reSVGTag.FindIndex(data); loc != nil {
		tag := append([]byte{}, data[loc[0]:loc[1]]...)
		tag = regexp.MustCompile(` width="[^"]*"`).ReplaceAll(tag,
			[]byte(fmt.Sprintf(` width="%d"`, size)))
		tag = regexp.MustCompile(` height="[^"]*"`).ReplaceAll(tag,
			[]byte(fmt.Sprintf(` height="%d"`, size)))
		data = append(append(data[:loc[0]:loc[0]], tag...), data[loc[1]:]...)
	}

	// Scale all numbers in path d= attributes.
	reD := regexp.MustCompile(`\bd="([^"]*)"`)
	data = reD.ReplaceAllFunc(data, func(match []byte) []byte {
		sub := reD.FindSubmatch(match)
		return []byte(`d="` + bakeScale(string(sub[1]), sx) + `"`)
	})

	// Scale circle / ellipse / line coordinate attributes.
	for _, attr := range []string{"cx", "cy", "r", "rx", "ry", "x1", "x2", "y1", "y2"} {
		re := regexp.MustCompile(`\b(` + attr + `)="([^"]*)"`)
		data = re.ReplaceAllFunc(data, func(match []byte) []byte {
			sub := re.FindSubmatch(match)
			return []byte(string(sub[1]) + `="` + bakeScale(string(sub[2]), sx) + `"`)
		})
	}

	// Scale stroke-width so it is proportional to the target size.
	reSW := regexp.MustCompile(`\bstroke-width="([^"]*)"`)
	data = reSW.ReplaceAllFunc(data, func(match []byte) []byte {
		sub := reSW.FindSubmatch(match)
		return []byte(`stroke-width="` + bakeScale(string(sub[1]), sx) + `"`)
	})

	return data
}

// RenderSVG reads the .svg file at path, rasterizes it to a square image of
// sizePixels×sizePixels, and returns the resulting RGBA image.
func RenderSVG(path string, sizePixels int) (*image.RGBA, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("RenderSVG: cannot open %q: %w", path, err)
	}

	// Rewrite CSS rgba() → hex (oksvg doesn't support rgba()).
	data := patchSVGColors(raw)

	// Bake all coordinates into the target pixel space so that oksvg/rasterx
	// receives 1:1 pixel coordinates without any viewport scaling.
	data = bakeCoords(data, sizePixels)

	icon, err := oksvg.ReadIconStream(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("RenderSVG: cannot parse SVG %q: %w", path, err)
	}

	w, h := sizePixels, sizePixels
	icon.SetTarget(0, 0, float64(w), float64(h))

	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	scanner := rasterx.NewScannerGV(w, h, rgba, rgba.Bounds())
	raster := rasterx.NewDasher(w, h, scanner)
	icon.Draw(raster, 1.0)

	return rgba, nil
}

// svgImageToRects converts opaque-enough pixels in img to xproto.Rectangles,
// offset so the image top-left maps to (offsetX, offsetY) on screen.
// Each horizontal run of pixels with alpha >= 128 becomes one rectangle.
// The alpha threshold prevents barely-visible anti-aliased edge pixels from
// being included in the XShape bounding region, which would show as dark
// splatters because X11 draws those pixels opaquely against the black window.
func svgImageToRects(img *image.RGBA, offsetX, offsetY int16) []xproto.Rectangle {
	bounds := img.Bounds()
	var rects []xproto.Rectangle

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		startX := -1
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			a := img.Pix[img.PixOffset(x, y)+3]
			if a >= 128 && startX < 0 {
				startX = x
			} else if a < 128 && startX >= 0 {
				rects = append(rects, xproto.Rectangle{
					X:      offsetX + int16(startX),
					Y:      offsetY + int16(y),
					Width:  uint16(x - startX),
					Height: 1,
				})
				startX = -1
			}
		}
		if startX >= 0 {
			rects = append(rects, xproto.Rectangle{
				X:      offsetX + int16(startX),
				Y:      offsetY + int16(y),
				Width:  uint16(bounds.Max.X - startX),
				Height: 1,
			})
		}
	}

	return rects
}
