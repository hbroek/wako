package img

import (
	"bytes"
	"encoding/binary"
)

// webpSize reads the canvas size from the RIFF header.
func webpSize(d []byte) (w, h int, ok bool) {
	if len(d) < 30 || string(d[:4]) != "RIFF" || string(d[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(d[12:16]) {
	case "VP8X":
		w = (int(d[24]) | int(d[25])<<8 | int(d[26])<<16) + 1
		h = (int(d[27]) | int(d[28])<<8 | int(d[29])<<16) + 1
	case "VP8L":
		if d[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(d[21:])
		w, h = int(bits&0x3fff)+1, int(bits>>14&0x3fff)+1
	case "VP8 ":
		if d[23] != 0x9d || d[24] != 0x01 || d[25] != 0x2a {
			return 0, 0, false
		}
		w = int(binary.LittleEndian.Uint16(d[26:]) & 0x3fff)
		h = int(binary.LittleEndian.Uint16(d[28:]) & 0x3fff)
	default:
		return 0, 0, false
	}
	return w, h, w > 0 && h > 0
}

// processWebP drops EXIF and XMP chunks. Pixels are never re-encoded (the
// standard library has no WebP encoder).
func processWebP(data []byte, lim Limits) (*Result, error) {
	if len(data) < 12 {
		return nil, ErrCorrupt
	}
	riffEnd := 8 + int(binary.LittleEndian.Uint32(data[4:]))
	if riffEnd > len(data) || riffEnd < 12 {
		return nil, ErrCorrupt
	}
	data = data[:riffEnd] // anything after the declared end is not part of the picture
	w, h, ok := webpSize(data)
	if !ok {
		return nil, ErrCorrupt
	}
	if err := checkPixels(w, h, lim); err != nil {
		return nil, err
	}
	out := bytes.NewBuffer(make([]byte, 0, riffEnd))
	out.Write(data[:12])
	for i := 12; i+8 <= riffEnd; {
		typ := string(data[i : i+4])
		n := int(binary.LittleEndian.Uint32(data[i+4:]))
		end := i + 8 + n + n&1 // chunks are padded to an even length
		if i+8+n > riffEnd {
			return nil, ErrCorrupt
		}
		if end > riffEnd {
			end = riffEnd
		}
		switch typ {
		case "EXIF", "XMP ":
		case "VP8X":
			if n < 10 {
				return nil, ErrCorrupt // flags, reserved bytes and the canvas size
			}
			chunk := append([]byte(nil), data[i:end]...)
			chunk[8] &^= 0x08 | 0x04 // EXIF and XMP flags
			out.Write(chunk)
		default:
			out.Write(data[i:end])
		}
		i = end
	}
	b := out.Bytes()
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return &Result{Data: b, Mime: "image/webp", Width: w, Height: h}, nil
}
