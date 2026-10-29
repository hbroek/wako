// Package img validates, cleans and shrinks uploaded images using only the
// standard library.
//
// Upload (Process) strips metadata (GPS, camera, thumbnails, comments,
// trailing data) while keeping the colour profile, applies EXIF orientation,
// and in Compact mode shrinks large pictures. Scale builds the smaller copies
// that are served on request.
package img

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF decoder for image.DecodeConfig
	"image/jpeg"
	"image/png"
	"regexp"
	"strconv"
)

var (
	ErrUnsupported   = errors.New("unsupported image type")
	ErrCorrupt       = errors.New("corrupt or undecodable image")
	ErrTooManyPixels = errors.New("image has too many pixels")
	ErrTooManyFrames = errors.New("gif has too many frames")
	ErrTooLarge      = errors.New("image is too large even after compaction")
)

// Mode selects how much Process changes a picture.
type Mode int

const (
	// Compact shrinks the longest side to Limits.MaxSide and keeps whichever is
	// smaller, the shrunk copy or the metadata-free original.
	Compact Mode = iota
	// Original keeps the picture as it is and only strips metadata.
	Original
)

// Limits bound what Process accepts and stores.
type Limits struct {
	MaxPixels int // decoded pixels; stops a small file unpacking into a huge bitmap
	MaxSide   int // longest side after Compact
	MaxStored int // bytes kept after processing
	MaxFrames int // GIF frames
	Mode      Mode
}

// DefaultLimits are the limits used when none are configured.
var DefaultLimits = Limits{MaxPixels: 24_000_000, MaxSide: 1600, MaxStored: 5 << 20, MaxFrames: 300}

// Result is a processed image ready to store.
type Result struct {
	Data          []byte
	Mime          string
	Width, Height int
}

// Process cleans data, which must already be known to be of type mime (decided
// from the content, not from the client).
func Process(data []byte, mime string, lim Limits) (*Result, error) {
	if lim.MaxPixels <= 0 {
		lim = DefaultLimits
	}
	var (
		res *Result
		err error
	)
	switch mime {
	case "image/jpeg":
		res, err = processJPEG(data, lim)
	case "image/png":
		res, err = processPNG(data, lim)
	case "image/gif":
		res, err = processGIF(data, lim)
	case "image/webp":
		res, err = processWebP(data, lim)
	case "image/svg+xml":
		w, h := svgDimensions(data)
		res = &Result{Data: data, Mime: mime, Width: w, Height: h}
	default:
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if lim.MaxStored > 0 && len(res.Data) > lim.MaxStored {
		return nil, fmt.Errorf("%w (%d bytes, limit %d)", ErrTooLarge, len(res.Data), lim.MaxStored)
	}
	return res, nil
}

// Dimensions reports the pixel size of an image, or 0, 0 if it cannot tell.
func Dimensions(mime string, data []byte) (w, h int) {
	switch mime {
	case "image/jpeg", "image/png", "image/gif":
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			return cfg.Width, cfg.Height
		}
	case "image/webp":
		if w, h, ok := webpSize(data); ok {
			return w, h
		}
	case "image/svg+xml":
		return svgDimensions(data)
	}
	return 0, 0
}

func checkPixels(w, h int, lim Limits) error {
	if w <= 0 || h <= 0 {
		return ErrCorrupt
	}
	if lim.MaxPixels > 0 && w*h > lim.MaxPixels {
		return fmt.Errorf("%w (%dx%d, limit %d)", ErrTooManyPixels, w, h, lim.MaxPixels)
	}
	return nil
}

func longest(w, h int) int {
	if w > h {
		return w
	}
	return h
}

// Fitted returns the size of a w x h picture scaled, keeping its proportions,
// so that its longest side is at most limit. It never enlarges.
func Fitted(w, h, limit int) (int, int) {
	m := longest(w, h)
	if limit <= 0 || m <= limit {
		return w, h
	}
	scale := func(v int) int {
		r := (v*limit + m/2) / m
		if r < 1 {
			r = 1
		}
		return r
	}
	return scale(w), scale(h)
}

// Scale returns a copy of a JPEG or PNG whose longest side is at most limit.
// The colour profile is kept. It does not change types.
func Scale(data []byte, mime string, limit int) (out []byte, w, h int, err error) {
	switch mime {
	case "image/jpeg":
		im, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, 0, 0, ErrCorrupt
		}
		b := im.Bounds()
		w, h = Fitted(b.Dx(), b.Dy(), limit)
		out, err = encodeJPEG(resize(im, w, h), 80, jpegICC(data))
		return out, w, h, err
	case "image/png":
		im, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, 0, 0, ErrCorrupt
		}
		b := im.Bounds()
		w, h = Fitted(b.Dx(), b.Dy(), limit)
		out, err = encodePNG(resize(im, w, h), pngColourChunks(data))
		return out, w, h, err
	}
	return nil, 0, 0, ErrUnsupported
}

// ---- SVG ----

var (
	svgTagRe  = regexp.MustCompile(`(?is)<svg\b[^>]*>`)
	svgAttrRe = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b` + name + `\s*=\s*["']\s*([0-9.]+)\s*(?:px)?\s*["']`)
	}
	svgW, svgH   = svgAttrRe("width"), svgAttrRe("height")
	svgViewBoxRe = regexp.MustCompile(`(?i)\bviewBox\s*=\s*["']\s*[-0-9.]+[ ,]+[-0-9.]+[ ,]+([0-9.]+)[ ,]+([0-9.]+)\s*["']`)
)

// svgDimensions reads width and height (or the viewBox) from the root element.
// It returns 0, 0 for sizes given in percentages or other units.
func svgDimensions(data []byte) (int, int) {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	tag := svgTagRe.Find(head)
	if tag == nil {
		return 0, 0
	}
	num := func(b []byte) int {
		f, err := strconv.ParseFloat(string(b), 64)
		if err != nil || f < 1 || f > 100000 {
			return 0
		}
		return int(f + 0.5)
	}
	if mw, mh := svgW.FindSubmatch(tag), svgH.FindSubmatch(tag); mw != nil && mh != nil {
		if w, h := num(mw[1]), num(mh[1]); w > 0 && h > 0 {
			return w, h
		}
	}
	if m := svgViewBoxRe.FindSubmatch(tag); m != nil {
		if w, h := num(m[1]), num(m[2]); w > 0 && h > 0 {
			return w, h
		}
	}
	return 0, 0
}
