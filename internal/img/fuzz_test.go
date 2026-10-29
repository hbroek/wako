package img

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func seedImages(f *testing.F) {
	g := image.NewGray(image.Rect(0, 0, 7, 5))
	for i := range g.Pix {
		g.Pix[i] = uint8(i * 5)
	}
	var j, p, gf bytes.Buffer
	jpeg.Encode(&j, g, nil)
	png.Encode(&p, g)
	gif.EncodeAll(&gf, &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White})}, Delay: []int{1}})
	exif := []byte{0xFF, 0xE1, 0x00, 0x24, 'E', 'x', 'i', 'f', 0, 0, 'I', 'I', '*', 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	f.Add(j.Bytes(), "image/jpeg")
	f.Add(append(append(append([]byte(nil), j.Bytes()[:2]...), exif...), j.Bytes()[2:]...), "image/jpeg")
	f.Add(p.Bytes(), "image/png")
	f.Add(gf.Bytes(), "image/gif")
	f.Add(j.Bytes()[:len(j.Bytes())/2], "image/jpeg")
	f.Add(p.Bytes()[:30], "image/png")
	f.Add(webpFileForFuzz(), "image/webp")
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="3" height="4"/>`), "image/svg+xml")
}

func webpFileForFuzz() []byte {
	// RIFF....WEBPVP8X with canvas 640x480, an EXIF chunk and a VP8L chunk.
	b := []byte("RIFF\x00\x00\x00\x00WEBP")
	b = append(b, "VP8X\x0a\x00\x00\x00\x0c\x00\x00\x00\x7f\x02\x00\xdf\x01\x00"...)
	b = append(b, "EXIF\x04\x00\x00\x00GPS!"...)
	b = append(b, "VP8L\x05\x00\x00\x00\x2f\x00\x00\x00\x00\x00"...)
	n := len(b) - 8
	b[4], b[5], b[6], b[7] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
	return b
}

// Process must survive any bytes, and what it accepts must be stable: cleaning
// a cleaned picture changes nothing, and it still decodes to the size reported.
func FuzzProcess(f *testing.F) {
	seedImages(f)
	lim := DefaultLimits
	lim.MaxPixels = 4_000_000 // keep each run quick
	f.Fuzz(func(t *testing.T, data []byte, mime string) {
		res, err := Process(data, mime, lim)
		if err != nil {
			return
		}
		if res.Width < 0 || res.Height < 0 || (res.Width == 0) != (res.Height == 0) && mime != "image/svg+xml" {
			t.Fatalf("odd size %dx%d", res.Width, res.Height)
		}
		if res.Mime != mime {
			t.Fatalf("type changed from %s to %s", mime, res.Mime)
		}
		w, h := Dimensions(res.Mime, res.Data)
		if w != res.Width || h != res.Height {
			t.Fatalf("reported %dx%d but the data says %dx%d", res.Width, res.Height, w, h)
		}
		again, err := Process(res.Data, res.Mime, lim)
		if err != nil {
			t.Fatalf("a cleaned picture is refused: %v", err)
		}
		if !bytes.Equal(again.Data, res.Data) {
			t.Fatalf("cleaning is not idempotent (%d -> %d bytes)", len(res.Data), len(again.Data))
		}
	})
}

func FuzzScale(f *testing.F) {
	seedImages(f)
	f.Fuzz(func(t *testing.T, data []byte, mime string) {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width*cfg.Height > 2_000_000 {
			return
		}
		out, w, h, err := Scale(data, mime, 3)
		if err != nil {
			return
		}
		if w > 3 || h > 3 || w < 1 || h < 1 {
			t.Fatalf("scaled to %dx%d", w, h)
		}
		if _, _, err := image.DecodeConfig(bytes.NewReader(out)); err != nil {
			t.Fatalf("the scaled picture does not decode: %v", err)
		}
	})
}
