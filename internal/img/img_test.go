package img

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// ---- builders ----

func solid(w, h int, c color.Color) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, c)
		}
	}
	return im
}

// gradient is a photo-like picture that compresses worse than a flat colour.
func gradient(w, h int) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.RGBA{uint8(x * 7 % 251), uint8(y * 13 % 241), uint8((x*y)%199 + 20), 255})
		}
	}
	return im
}

func jpegBytes(t *testing.T, im image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngBytes(t *testing.T, im image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func segment(marker byte, payload []byte) []byte {
	n := len(payload) + 2
	return append([]byte{0xFF, marker, byte(n >> 8), byte(n)}, payload...)
}

func exifSegment(orientation int, secret string) []byte {
	tiff := []byte("II*\x00\x08\x00\x00\x00")
	tiff = binary.LittleEndian.AppendUint16(tiff, 1) // one entry
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3)
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, uint16(orientation))
	tiff = append(tiff, 0, 0, 0, 0, 0, 0) // padding and next-IFD offset
	tiff = append(tiff, secret...)
	return segment(0xE1, append([]byte("Exif\x00\x00"), tiff...))
}

func iccSegment() []byte {
	return segment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), bytes.Repeat([]byte{0x42}, 64)...))
}

// withSegments inserts extra segments straight after SOI.
func withSegments(jpg []byte, segs ...[]byte) []byte {
	out := append([]byte(nil), jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// withPNGChunks inserts chunks right after IHDR.
func withPNGChunks(p []byte, chunks ...[]byte) []byte {
	const at = 8 + 12 + 13
	out := append([]byte(nil), p[:at]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[at:]...)
}

func size(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("result does not decode: %v", err)
	}
	return cfg.Width, cfg.Height
}

// noise is incompressible, so a shrunk copy is certain to be smaller.
func noise(w, h int) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i], im.Pix[i+1], im.Pix[i+2], im.Pix[i+3] = uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255
	}
	return im
}

// ---- JPEG ----

func TestJPEGStripsMetadataKeepsProfile(t *testing.T) {
	base := jpegBytes(t, gradient(64, 48))
	dirty := withSegments(base,
		exifSegment(1, "GPSLatitude=52.37"),
		segment(0xFE, []byte("shot with a secret camera")),
		segment(0xED, []byte("Photoshop 3.0\x00private")),
		segment(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<xmp>secret</xmp>")),
		iccSegment(),
	)
	dirty = append(dirty, []byte("TRAILING-SECRET")...)

	res, err := Process(dirty, "image/jpeg", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"GPSLatitude", "secret camera", "Photoshop", "xmp", "TRAILING", "Exif"} {
		if bytes.Contains(res.Data, []byte(secret)) {
			t.Errorf("%q survived cleaning", secret)
		}
	}
	if !bytes.Contains(res.Data, []byte("ICC_PROFILE")) {
		t.Error("colour profile was dropped")
	}
	if w, h := size(t, res.Data); w != 64 || h != 48 || res.Width != 64 || res.Height != 48 {
		t.Fatalf("size %dx%d (result says %dx%d)", w, h, res.Width, res.Height)
	}
	if len(res.Data) >= len(dirty) {
		t.Error("cleaned file is not smaller")
	}
	if _, err := jpeg.Decode(bytes.NewReader(res.Data)); err != nil {
		t.Fatalf("cleaned JPEG does not decode: %v", err)
	}
	// Pixels are untouched: no recompression happened.
	if !bytes.Contains(res.Data, base[len(base)/2:len(base)-2]) {
		t.Error("image data changed although only metadata was removed")
	}
}

func TestJPEGOrientationIsAppliedOnce(t *testing.T) {
	src := solid(40, 20, color.RGBA{0, 0, 255, 255})
	for y := 0; y < 6; y++ { // red patch in the top-left corner
		for x := 0; x < 6; x++ {
			src.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	data := withSegments(jpegBytes(t, src), exifSegment(6, "x"), iccSegment())
	res, err := Process(data, "image/jpeg", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 20 || res.Height != 40 {
		t.Fatalf("rotated size %dx%d, want 20x40", res.Width, res.Height)
	}
	if bytes.Contains(res.Data, []byte("Exif")) || !bytes.Contains(res.Data, []byte("ICC_PROFILE")) {
		t.Error("rotated file should have no EXIF but keep its profile")
	}
	im, _ := jpeg.Decode(bytes.NewReader(res.Data))
	// Orientation 6 turns the picture 90 degrees clockwise: top-left goes to top-right.
	r, g, b, _ := im.At(17, 2).RGBA()
	if r>>8 < 200 || g>>8 > 60 || b>>8 > 60 {
		t.Errorf("red patch not at the top-right after rotation: %v %v %v", r>>8, g>>8, b>>8)
	}
	r, _, _, _ = im.At(2, 2).RGBA()
	if r>>8 > 100 {
		t.Error("top-left should now be blue")
	}
}

func TestOrientTable(t *testing.T) {
	// A 3x2 image with distinct pixels; after each orientation the pixel that was
	// at the source top-left must be where the EXIF spec says.
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := 0; i < 6; i++ {
		src.Set(i%3, i/3, color.RGBA{uint8(i + 1), 0, 0, 255})
	}
	topLeft := map[int][2]int{2: {2, 0}, 3: {2, 1}, 4: {0, 1}, 5: {0, 0}, 6: {1, 0}, 7: {1, 2}, 8: {0, 2}}
	for o, at := range topLeft {
		got := orient(src, o).(*image.RGBA)
		if r := got.RGBAAt(at[0], at[1]).R; r != 1 {
			t.Errorf("orientation %d: source top-left (value 1) is not at %v (found %d)", o, at, r)
		}
	}
}

func TestJPEGCompactShrinksAndKeepsSmaller(t *testing.T) {
	big := jpegBytes(t, gradient(2400, 1200))
	lim := DefaultLimits
	res, err := Process(big, "image/jpeg", lim)
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 1600 || res.Height != 800 {
		t.Fatalf("compact size %dx%d, want 1600x800", res.Width, res.Height)
	}
	if w, h := size(t, res.Data); w != 1600 || h != 800 {
		t.Fatalf("decoded %dx%d", w, h)
	}
	if len(res.Data) >= len(big) {
		t.Errorf("shrunk copy (%d) not smaller than original (%d)", len(res.Data), len(big))
	}

	// Original mode only strips.
	lim.Mode = Original
	res, err = Process(big, "image/jpeg", lim)
	if err != nil || res.Width != 2400 {
		t.Fatalf("original mode: %v %+v", err, res)
	}

	// A flat colour is already tiny: shrinking it must not make it bigger.
	flat := jpegBytes(t, solid(1700, 1700, color.RGBA{10, 20, 30, 255}))
	res, err = Process(flat, "image/jpeg", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data) > len(flat) {
		t.Errorf("result (%d) larger than the original (%d)", len(res.Data), len(flat))
	}
	// Within the limit nothing changes.
	small := jpegBytes(t, gradient(300, 200))
	if res, _ := Process(small, "image/jpeg", DefaultLimits); res.Width != 300 || res.Height != 200 {
		t.Errorf("small image changed: %+v", res)
	}
}

func TestLimitsAndCorruption(t *testing.T) {
	flat := pngBytes(t, solid(4000, 4000, color.RGBA{1, 2, 3, 255})) // tiny file, 16 Mpx
	lim := DefaultLimits
	lim.MaxPixels = 10_000_000
	if _, err := Process(flat, "image/png", lim); !errors.Is(err, ErrTooManyPixels) {
		t.Fatalf("pixel limit: %v", err)
	}
	lim = DefaultLimits
	lim.MaxStored = 100
	if _, err := Process(jpegBytes(t, gradient(100, 100)), "image/jpeg", lim); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("stored limit: %v", err)
	}
	jpg := jpegBytes(t, gradient(50, 50))
	for name, data := range map[string][]byte{
		"garbage": []byte("not an image at all"), "truncated": jpg[:len(jpg)/3], "empty": nil,
	} {
		if _, err := Process(data, "image/jpeg", DefaultLimits); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s jpeg: %v", name, err)
		}
	}
	if _, err := Process([]byte("x"), "text/plain", DefaultLimits); !errors.Is(err, ErrUnsupported) {
		t.Error("unsupported type accepted")
	}
}

// ---- PNG ----

func TestPNGStripsTextKeepsColour(t *testing.T) {
	base := pngBytes(t, gradient(40, 30))
	icc := pngChunk("iCCP", append([]byte("profile\x00\x00"), bytes.Repeat([]byte{1}, 20)...))
	dirty := withPNGChunks(base,
		pngChunk("tEXt", []byte("Comment\x00secret note")),
		pngChunk("eXIf", []byte("GPS-DATA")),
		pngChunk("tIME", make([]byte, 7)),
		icc,
	)
	dirty = append(dirty, "TRAILING"...)
	res, err := Process(dirty, "image/png", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"tEXt", "secret note", "eXIf", "GPS-DATA", "tIME", "TRAILING"} {
		if bytes.Contains(res.Data, []byte(s)) {
			t.Errorf("%q survived", s)
		}
	}
	if !bytes.Contains(res.Data, []byte("iCCP")) {
		t.Error("colour profile dropped")
	}
	if w, h := size(t, res.Data); w != 40 || h != 30 {
		t.Fatalf("size %dx%d", w, h)
	}
	if _, err := png.Decode(bytes.NewReader(res.Data)); err != nil {
		t.Fatal(err)
	}
}

func TestPNGCompactAndAnimated(t *testing.T) {
	big := withPNGChunks(pngBytes(t, noise(2000, 1000)), pngChunk("iCCP", append([]byte("p\x00\x00"), 9, 9, 9)))
	res, err := Process(big, "image/png", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 1600 || res.Height != 800 {
		t.Fatalf("size %dx%d", res.Width, res.Height)
	}
	if !bytes.Contains(res.Data, []byte("iCCP")) {
		t.Error("profile lost when shrinking")
	}
	if len(res.Data) > len(big) {
		t.Error("larger than the original")
	}
	// An APNG decodes as one frame, so it must never be shrunk.
	anim := withPNGChunks(pngBytes(t, gradient(2000, 1000)), pngChunk("acTL", make([]byte, 8)))
	res, err = Process(anim, "image/png", DefaultLimits)
	if err != nil || res.Width != 2000 {
		t.Fatalf("animated png was changed: %v %+v", err, res)
	}
	if !bytes.Contains(res.Data, []byte("acTL")) {
		t.Error("animation chunk dropped")
	}
	// Transparency survives shrinking.
	alpha := image.NewNRGBA(image.Rect(0, 0, 2000, 1000))
	for i := 3; i < len(alpha.Pix); i += 4 {
		alpha.Pix[i] = 0
	}
	res, err = Process(pngBytes(t, alpha), "image/png", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	im, _ := png.Decode(bytes.NewReader(res.Data))
	if _, _, _, a := im.At(10, 10).RGBA(); a != 0 {
		t.Errorf("transparent pixel became opaque: alpha %d", a)
	}
}

// ---- GIF ----

func gifBytes(t *testing.T, frames int) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0}
	pal := color.Palette{color.Black, color.White}
	for i := 0; i < frames; i++ {
		f := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
		f.SetColorIndex(i%8, i%8, 1)
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 5)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestGIFStripsCommentsKeepsAnimation(t *testing.T) {
	g := gifBytes(t, 3)
	// Comment extension straight after the global colour table.
	at := 13
	if g[10]&0x80 != 0 {
		at += 3 << (uint(g[10]&7) + 1)
	}
	comment := append([]byte{0x21, 0xFE, 11}, "secret note"...)
	comment = append(comment, 0)
	dirty := append(append(append([]byte(nil), g[:at]...), comment...), g[at:]...)
	dirty = append(dirty, "TRAILING"...)

	res, err := Process(dirty, "image/gif", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(res.Data, []byte("secret note")) || bytes.Contains(res.Data, []byte("TRAILING")) {
		t.Error("comment or trailing data survived")
	}
	if !bytes.Contains(res.Data, []byte("NETSCAPE2.0")) {
		t.Error("loop extension dropped")
	}
	all, err := gif.DecodeAll(bytes.NewReader(res.Data))
	if err != nil || len(all.Image) != 3 || all.Delay[0] != 5 {
		t.Fatalf("animation damaged: %v frames=%d", err, len(all.Image))
	}
	lim := DefaultLimits
	lim.MaxFrames = 2
	if _, err := Process(dirty, "image/gif", lim); !errors.Is(err, ErrTooManyFrames) {
		t.Fatalf("frame cap: %v", err)
	}
}

// ---- WebP and SVG ----

func webpFile(w, h int, extra ...[]byte) []byte {
	chunk := func(typ string, payload []byte) []byte {
		b := append([]byte(typ), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
		b = append(b, payload...)
		if len(payload)%2 == 1 {
			b = append(b, 0)
		}
		return b
	}
	vp8x := []byte{0x0C, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)}
	body := []byte("WEBP")
	body = append(body, chunk("VP8X", vp8x)...)
	body = append(body, chunk("EXIF", []byte("GPS-SECRET"))...)
	body = append(body, chunk("XMP ", []byte("<xmp>secret</xmp>"))...)
	body = append(body, chunk("ICCP", []byte("profile!"))...)
	body = append(body, chunk("VP8L", bytes.Repeat([]byte{7}, 21))...)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

func TestWebPStripsExifAndXMP(t *testing.T) {
	res, err := Process(webpFile(640, 480), "image/webp", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(res.Data, []byte("GPS-SECRET")) || bytes.Contains(res.Data, []byte("xmp")) {
		t.Error("metadata chunk survived")
	}
	if !bytes.Contains(res.Data, []byte("ICCP")) {
		t.Error("profile dropped")
	}
	if res.Width != 640 || res.Height != 480 {
		t.Fatalf("size %dx%d", res.Width, res.Height)
	}
	if got := int(binary.LittleEndian.Uint32(res.Data[4:])); got != len(res.Data)-8 {
		t.Errorf("RIFF size %d, want %d", got, len(res.Data)-8)
	}
	if res.Data[20]&0x0C != 0 {
		t.Error("EXIF/XMP flags still set")
	}
	lim := DefaultLimits
	lim.MaxPixels = 1000
	if _, err := Process(webpFile(640, 480), "image/webp", lim); !errors.Is(err, ErrTooManyPixels) {
		t.Errorf("webp pixel limit: %v", err)
	}
	if _, err := Process([]byte("RIFFxxxxWEBPjunk"), "image/webp", DefaultLimits); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad webp: %v", err)
	}
}

func TestSVGDimensions(t *testing.T) {
	cases := map[string][2]int{
		`<svg xmlns="http://www.w3.org/2000/svg" width="120" height="80px"/>`:   {120, 80},
		`<?xml version="1.0"?><svg viewBox="0 0 300.5 150" width="100%"></svg>`: {301, 150},
		`<svg width="50%" height="50%"/>`:                                       {0, 0},
		`<svg/>`:                                                                {0, 0},
	}
	for src, want := range cases {
		if w, h := svgDimensions([]byte(src)); [2]int{w, h} != want {
			t.Errorf("%s: got %dx%d, want %v", src, w, h, want)
		}
	}
}

// ---- resizing ----

func TestFitted(t *testing.T) {
	for _, c := range []struct{ w, h, limit, fw, fh int }{
		{4000, 3000, 800, 800, 600},
		{3000, 4000, 800, 600, 800},
		{800, 600, 800, 800, 600},
		{500, 400, 800, 500, 400},
		{10000, 3, 800, 800, 1},
	} {
		if fw, fh := Fitted(c.w, c.h, c.limit); fw != c.fw || fh != c.fh {
			t.Errorf("Fitted(%d,%d,%d) = %d,%d want %d,%d", c.w, c.h, c.limit, fw, fh, c.fw, c.fh)
		}
	}
}

func TestResizeAveragesArea(t *testing.T) {
	// A 1-pixel black/white checkerboard averages to mid grey.
	src := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			v := uint8(0)
			if (x+y)%2 == 0 {
				v = 255
			}
			src.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	out := resize(src, 10, 10)
	if out.Rect.Dx() != 10 || out.Rect.Dy() != 10 {
		t.Fatalf("size %v", out.Rect)
	}
	for _, p := range []image.Point{{0, 0}, {5, 5}, {9, 9}} {
		if v := out.RGBAAt(p.X, p.Y).R; v < 120 || v > 135 {
			t.Errorf("pixel %v = %d, want ~127", p, v)
		}
	}
	// Flat colours stay flat at awkward ratios, and odd sizes do not panic.
	flat := resize(solid(333, 217, color.RGBA{50, 100, 150, 255}), 71, 47)
	for _, p := range []image.Point{{0, 0}, {70, 46}, {35, 20}} {
		if c := flat.RGBAAt(p.X, p.Y); c.R != 50 || c.G != 100 || c.B != 150 || c.A != 255 {
			t.Errorf("flat pixel %v = %v", p, c)
		}
	}
	// It never enlarges.
	if got := resize(src, 100, 100); got.Rect.Dx() != 40 {
		t.Errorf("enlarged to %v", got.Rect)
	}
}

func TestScaleKeepsProfileAndType(t *testing.T) {
	jpg := withSegments(jpegBytes(t, gradient(1200, 900)), iccSegment())
	out, w, h, err := Scale(jpg, "image/jpeg", 800)
	if err != nil || w != 800 || h != 600 {
		t.Fatalf("jpeg scale: %v %dx%d", err, w, h)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil || !bytes.Contains(out, []byte("ICC_PROFILE")) {
		t.Errorf("jpeg variant: decode err %v, profile kept %v", err, bytes.Contains(out, []byte("ICC_PROFILE")))
	}
	p := withPNGChunks(pngBytes(t, gradient(900, 1200)), pngChunk("iCCP", append([]byte("p\x00\x00"), 1, 2, 3)))
	out, w, h, err = Scale(p, "image/png", 800)
	if err != nil || w != 600 || h != 800 {
		t.Fatalf("png scale: %v %dx%d", err, w, h)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil || !bytes.Contains(out, []byte("iCCP")) {
		t.Errorf("png variant: %v", err)
	}
	if _, _, _, err := Scale(gifBytes(t, 1), "image/gif", 800); !errors.Is(err, ErrUnsupported) {
		t.Errorf("gif must not be scalable: %v", err)
	}
}

func TestPDFsHaveTheirOwnStoredLimit(t *testing.T) {
	pdf := append([]byte("%PDF-1.4\n"), make([]byte, 7<<20)...) // over the 5 MiB image limit, under the 10 MiB PDF limit
	if _, err := Process(pdf, "application/pdf", DefaultLimits); err != nil {
		t.Fatalf("7 MiB pdf: %v", err)
	}
	if _, err := Process(append(pdf, make([]byte, 4<<20)...), "application/pdf", DefaultLimits); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("11 MiB pdf: %v", err)
	}
	lim := DefaultLimits
	lim.MaxPDF = 0 // falls back to the image limit
	if _, err := Process(pdf, "application/pdf", lim); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("pdf without own limit: %v", err)
	}
}
