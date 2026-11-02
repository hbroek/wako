package server

import "testing"

func TestDropNewImagesKeepsPicturesAlreadyOnThePage(t *testing.T) {
	page := "| ![Logo](https://example.com/logo.svg#h60) | x |\n![Photo](https://example.com/p.jpg)\n"
	for _, c := range []struct{ in, want string }{
		// Same picture, size added, changed or removed.
		{"![Logo](https://example.com/logo.svg#h=60)", "![Logo](https://example.com/logo.svg#h=60)"},
		{"![Photo](https://example.com/p.jpg#w=400)", "![Photo](https://example.com/p.jpg#w=400)"},
		{"![Logo](https://example.com/logo.svg)", "![Logo](https://example.com/logo.svg)"},
		// An address that was not there is dropped to its description, also when it extends one that was.
		{"![x](https://evil.example/c?d=secret)", "x"},
		{"![Photo](https://example.com/p.jpg?leak=1#w=400)", "Photo"},
		{"![x](//evil.example/a.png)", "x"},
		{"![ok](local.png#w=400)", "![ok](local.png#w=400)"},
	} {
		if got := dropNewImages(c.in, page); got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}
