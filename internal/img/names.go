package img

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
)

// Ext maps the accepted image types to the file extension used in names.
var Ext = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/svg+xml":   ".svg",
	"application/pdf": ".pdf", // an attachment, not an image: kept as uploaded
}

// Sniff decides the type from the content, never from what a client claims.
// SVG is text, so it also needs a ".svg" name and an <svg element.
func Sniff(data []byte, name string) (mime string, ok bool) {
	ct, _, _ := strings.Cut(http.DetectContentType(data), ";")
	if _, ok := Ext[ct]; ok {
		return ct, true
	}
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	if strings.HasSuffix(strings.ToLower(name), ".svg") && bytes.Contains(bytes.ToLower(head), []byte("<svg")) {
		return "image/svg+xml", true
	}
	return "", false
}

var badNameChar = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// CleanName returns a URL-safe library file name whose extension matches the
// type: "My Shot!.PNG" becomes "My-Shot.png".
func CleanName(name, mime string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	name = strings.Trim(badNameChar.ReplaceAllString(name, "-"), "-._")
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "image"
	}
	return name + Ext[mime]
}
