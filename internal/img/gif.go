package img

import (
	"bytes"
	"image/gif"
)

// processGIF strips comments, plain-text blocks and unknown application
// extensions. Animation, and therefore every frame, is kept untouched, so a GIF
// is never shrunk; it is rejected when it has more than Limits.MaxFrames frames.
func processGIF(data []byte, lim Limits) (*Result, error) {
	cfg, err := gif.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	if err := checkPixels(cfg.Width, cfg.Height, lim); err != nil {
		return nil, err
	}
	if len(data) < 13 {
		return nil, ErrCorrupt
	}
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write(data[:13]) // header and logical screen descriptor
	i := 13
	if data[10]&0x80 != 0 {
		n := 3 << (uint(data[10]&7) + 1)
		if i+n > len(data) {
			return nil, ErrCorrupt
		}
		out.Write(data[i : i+n])
		i += n
	}
	// subBlocks returns the end of a run of data sub-blocks starting at i.
	subBlocks := func(i int) (int, bool) {
		for i < len(data) {
			n := int(data[i])
			i += 1 + n
			if n == 0 {
				return i, i <= len(data)
			}
		}
		return i, false
	}
	frames := 0
	for i < len(data) {
		switch data[i] {
		case 0x3B: // trailer
			out.WriteByte(0x3B)
			return &Result{Data: out.Bytes(), Mime: "image/gif", Width: cfg.Width, Height: cfg.Height}, nil
		case 0x21: // extension
			if i+2 > len(data) {
				return nil, ErrCorrupt
			}
			label := data[i+1]
			end, ok := subBlocks(i + 2)
			if !ok {
				return nil, ErrCorrupt
			}
			keep := label == 0xF9 // graphic control: delay, transparency, disposal
			if label == 0xFF && i+3+11 <= len(data) {
				switch string(data[i+3 : i+3+11]) {
				case "NETSCAPE2.0", "ANIMEXTS1.0", "ICCRGBG1012": // loop count, colour profile
					keep = true
				}
			}
			if keep {
				out.Write(data[i:end])
			}
			i = end
		case 0x2C: // image
			frames++
			if lim.MaxFrames > 0 && frames > lim.MaxFrames {
				return nil, ErrTooManyFrames
			}
			if i+10 > len(data) {
				return nil, ErrCorrupt
			}
			start := i
			flags := data[i+9]
			i += 10
			if flags&0x80 != 0 {
				i += 3 << (uint(flags&7) + 1)
			}
			i++ // LZW minimum code size
			end, ok := subBlocks(i)
			if !ok || i > len(data) {
				return nil, ErrCorrupt
			}
			out.Write(data[start:end])
			i = end
		default:
			return nil, ErrCorrupt
		}
	}
	return nil, ErrCorrupt
}
