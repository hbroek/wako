package img

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
)

var pngSig = []byte("\x89PNG\r\n\x1a\n")

// pngKeep lists the chunks that survive cleaning: everything needed to decode,
// colour information, and APNG animation. Text, EXIF, timestamps and unknown
// ancillary chunks are dropped.
var pngKeep = map[string]bool{
	"IHDR": true, "PLTE": true, "IDAT": true, "IEND": true, "tRNS": true,
	"iCCP": true, "sRGB": true, "gAMA": true, "cHRM": true, "cICP": true, "sBIT": true, "bKGD": true,
	"acTL": true, "fcTL": true, "fdAT": true,
}

// pngColour lists the chunks a recompressed PNG needs to look the same.
var pngColour = map[string]bool{"iCCP": true, "sRGB": true, "gAMA": true, "cHRM": true, "cICP": true}

type pngParts struct {
	clean    []byte
	colour   [][]byte // whole chunks, in original order
	animated bool
}

func parsePNG(data []byte) (*pngParts, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], pngSig) {
		return nil, ErrCorrupt
	}
	p := &pngParts{}
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write(pngSig)
	i := 8
	for i+12 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[i:]))
		if n < 0 || i+12+n > len(data) {
			return nil, ErrCorrupt
		}
		typ := string(data[i+4 : i+8])
		chunk := data[i : i+12+n]
		if pngKeep[typ] {
			out.Write(chunk)
		}
		if pngColour[typ] {
			p.colour = append(p.colour, append([]byte(nil), chunk...))
		}
		if typ == "acTL" {
			p.animated = true
		}
		i += 12 + n
		if typ == "IEND" { // anything after IEND is trailing data
			p.clean = out.Bytes()
			return p, nil
		}
	}
	return nil, ErrCorrupt
}

func pngColourChunks(data []byte) [][]byte {
	p, err := parsePNG(data)
	if err != nil {
		return nil
	}
	return p.colour
}

// encodePNG encodes im and puts the colour chunks back after IHDR.
func encodePNG(im image.Image, colour [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, im); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(colour) == 0 {
		return b, nil
	}
	const ihdrEnd = 8 + 12 + 13 // signature + IHDR chunk
	out := make([]byte, 0, len(b)+256)
	out = append(out, b[:ihdrEnd]...)
	for _, c := range colour {
		out = append(out, c...)
	}
	return append(out, b[ihdrEnd:]...), nil
}

func processPNG(data []byte, lim Limits) (*Result, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	if err := checkPixels(cfg.Width, cfg.Height, lim); err != nil {
		return nil, err
	}
	p, err := parsePNG(data)
	if err != nil {
		return nil, err
	}
	keep := &Result{Data: p.clean, Mime: "image/png", Width: cfg.Width, Height: cfg.Height}
	// Decoding an animated PNG yields only the first frame, so never shrink one.
	if lim.Mode != Compact || lim.MaxSide <= 0 || p.animated || longest(cfg.Width, cfg.Height) <= lim.MaxSide {
		return keep, nil
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	w, h := Fitted(cfg.Width, cfg.Height, lim.MaxSide)
	enc, err := encodePNG(resize(im, w, h), p.colour)
	if err != nil || len(enc) >= len(p.clean) {
		return keep, nil
	}
	return &Result{Data: enc, Mime: "image/png", Width: w, Height: h}, nil
}
