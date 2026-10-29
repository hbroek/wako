package img

import (
	"image"
	"image/draw"
)

// toRGBA returns im as premultiplied RGBA with its origin at 0, 0.
func toRGBA(im image.Image) *image.RGBA {
	if r, ok := im.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
		return r
	}
	b := im.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Rect, im, b.Min, draw.Src)
	return dst
}

type weight struct {
	i int
	w float32
}

// axisWeights lists, for every destination index, the source indexes it covers
// and how much of each (box filter: area averaging).
func axisWeights(src, dst int) [][]weight {
	scale := float64(src) / float64(dst)
	out := make([][]weight, dst)
	for d := range out {
		start, end := float64(d)*scale, float64(d+1)*scale
		for s := int(start); s < src && float64(s) < end; s++ {
			lo, hi := start, end
			if float64(s) > lo {
				lo = float64(s)
			}
			if float64(s+1) < hi {
				hi = float64(s + 1)
			}
			if hi > lo {
				out[d] = append(out[d], weight{s, float32((hi - lo) / scale)})
			}
		}
	}
	return out
}

// resize shrinks im to dw x dh by area averaging. It works one source row at a
// time, so memory is the source plus one float accumulator per output pixel.
func resize(im image.Image, dw, dh int) *image.RGBA {
	src := toRGBA(im)
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if dw >= sw && dh >= sh {
		return src
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	xs := axisWeights(sw, dw)
	ys := axisWeights(sh, dh)
	// Invert the vertical weights: which output rows does each source row feed?
	type rowW struct {
		d int
		w float32
	}
	feeds := make([][]rowW, sh)
	for d, ws := range ys {
		for _, w := range ws {
			feeds[w.i] = append(feeds[w.i], rowW{d, w.w})
		}
	}
	acc := make([]float32, dw*dh*4)
	hrow := make([]float32, dw*4)
	for sy := 0; sy < sh; sy++ {
		row := src.Pix[sy*src.Stride : sy*src.Stride+sw*4]
		for dx, ws := range xs {
			var r, g, b, a float32
			for _, w := range ws {
				p := row[w.i*4 : w.i*4+4]
				r += w.w * float32(p[0])
				g += w.w * float32(p[1])
				b += w.w * float32(p[2])
				a += w.w * float32(p[3])
			}
			hrow[dx*4], hrow[dx*4+1], hrow[dx*4+2], hrow[dx*4+3] = r, g, b, a
		}
		for _, f := range feeds[sy] {
			dst := acc[f.d*dw*4 : (f.d+1)*dw*4]
			for k, v := range hrow {
				dst[k] += f.w * v
			}
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for k, v := range acc {
		if v > 255 {
			v = 255
		}
		out.Pix[k] = uint8(v + 0.5)
	}
	return out
}

// orient applies an EXIF orientation (2..8) so the pixels display upright.
func orient(im image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return im
	}
	src := toRGBA(im)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			copy(dst.Pix[y*dst.Stride+x*4:y*dst.Stride+x*4+4], src.Pix[sy*src.Stride+sx*4:sy*src.Stride+sx*4+4])
		}
	}
	return dst
}
