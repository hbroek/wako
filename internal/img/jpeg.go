package img

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
)

// jpegParts is a JPEG taken apart: the cleaned bytes, the ICC profile
// segments, and the EXIF orientation (1 if absent).
type jpegParts struct {
	clean       []byte
	icc         [][]byte // complete APP2 segments, marker included
	orientation int
}

// parseJPEG walks the marker segments and keeps only what decoding needs plus
// the colour profile. It drops EXIF, XMP, comments, thumbnails, vendor blocks
// and anything after the end-of-image marker.
func parseJPEG(data []byte) (*jpegParts, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, ErrCorrupt
	}
	p := &jpegParts{orientation: 1}
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write(data[:2])
	i := 2
	for i < len(data) {
		if data[i] != 0xFF { // stray byte between segments
			i++
			continue
		}
		for i < len(data) && data[i] == 0xFF { // fill bytes
			i++
		}
		if i >= len(data) {
			break
		}
		marker := data[i]
		i++
		switch {
		case marker == 0xD9: // EOI: everything after is trailing data
			out.Write([]byte{0xFF, 0xD9})
			return p.finish(out)
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8) || marker == 0x00:
			continue // standalone markers carry no length
		}
		if i+2 > len(data) {
			return nil, ErrCorrupt
		}
		n := int(binary.BigEndian.Uint16(data[i:]))
		if n < 2 || i+n > len(data) {
			return nil, ErrCorrupt
		}
		seg := data[i-2 : i+n] // FF marker length payload
		payload := data[i+2 : i+n]
		keep := true
		switch {
		case marker == 0xFE: // COM
			keep = false
		case marker >= 0xE0 && marker <= 0xEF: // APPn
			switch {
			case marker == 0xE1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")):
				if o := exifOrientation(payload[6:]); o != 0 {
					p.orientation = o
				}
				keep = false
			case marker == 0xE2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")):
				p.icc = append(p.icc, append([]byte(nil), seg...))
				keep = true
			case marker == 0xEE && bytes.HasPrefix(payload, []byte("Adobe")):
				keep = true // colour transform, needed to decode
			case marker == 0xE0 && bytes.HasPrefix(payload, []byte("JFIF\x00")) && len(payload) >= 14 && payload[12] == 0 && payload[13] == 0:
				keep = true // JFIF header without an embedded thumbnail
			default:
				keep = false
			}
		}
		if keep {
			out.Write(seg)
		}
		i += n
		if marker == 0xDA { // SOS: entropy-coded data follows, up to the next real marker
			start := i
			for i+1 < len(data) {
				if data[i] == 0xFF && data[i+1] != 0x00 && !(data[i+1] >= 0xD0 && data[i+1] <= 0xD7) {
					break
				}
				i++
			}
			out.Write(data[start:i])
		}
	}
	return nil, ErrCorrupt // no end-of-image marker: the file is truncated
}

func (p *jpegParts) finish(out *bytes.Buffer) (*jpegParts, error) {
	p.clean = out.Bytes()
	return p, nil
}

// exifOrientation reads the orientation tag from a TIFF block (0 if absent).
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:]) != 42 {
		return 0
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

// jpegICC returns the colour-profile segments of a JPEG.
func jpegICC(data []byte) [][]byte {
	p, err := parseJPEG(data)
	if err != nil {
		return nil
	}
	return p.icc
}

func encodeJPEG(im image.Image, quality int, icc [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(icc) == 0 {
		return b, nil
	}
	// The encoder starts with SOI; the profile goes straight after it.
	out := make([]byte, 0, len(b)+1024)
	out = append(out, b[:2]...)
	for _, seg := range icc {
		out = append(out, seg...)
	}
	return append(out, b[2:]...), nil
}

func processJPEG(data []byte, lim Limits) (*Result, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	if err := checkPixels(cfg.Width, cfg.Height, lim); err != nil {
		return nil, err
	}
	p, err := parseJPEG(data)
	if err != nil {
		return nil, err
	}
	rotate := p.orientation > 1
	shrink := lim.Mode == Compact && lim.MaxSide > 0 && longest(cfg.Width, cfg.Height) > lim.MaxSide
	w, h := cfg.Width, cfg.Height
	if !rotate && !shrink {
		return &Result{Data: p.clean, Mime: "image/jpeg", Width: w, Height: h}, nil
	}

	im, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	quality := 80
	if rotate {
		// The orientation flag is metadata, so the pixels are rotated and
		// recompressed once, at higher quality.
		im = orient(im, p.orientation)
		quality = 92
	}
	b := im.Bounds()
	w, h = b.Dx(), b.Dy()
	if shrink {
		w, h = Fitted(w, h, lim.MaxSide)
		im = resize(im, w, h)
	}
	enc, err := encodeJPEG(im, quality, p.icc)
	if err != nil {
		return nil, err
	}
	if !rotate && len(p.clean) <= len(enc) { // the shrunk copy was not smaller
		cw, ch := cfg.Width, cfg.Height
		return &Result{Data: p.clean, Mime: "image/jpeg", Width: cw, Height: ch}, nil
	}
	return &Result{Data: enc, Mime: "image/jpeg", Width: w, Height: h}, nil
}
