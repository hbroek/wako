package store

import (
	"fmt"
	"strings"
	"testing"
)

const benchPages = 10000

// benchSpace fills a space with benchPages pages: 100 sections of 100 pages.
func benchSpace(b *testing.B) *Space {
	b.Helper()
	st, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	sp, err := st.CreateSpace("bench", "Bench", true)
	if err != nil {
		b.Fatal(err)
	}
	body := strings.Repeat("The quick brown fox jumps over the lazy dog. See [other](../other) for more.\n\n", 20)
	for i := 0; i < benchPages/100; i++ {
		sec := fmt.Sprintf("sec%d", i)
		if _, _, err := sp.PutPage(sec, "Section "+sec, "# "+sec+"\n", Refs{}, Cond{}); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 99; j++ {
			text := fmt.Sprintf("# Page %d.%d\n\n%s\nunique%dx%d\n", i, j, body, i, j)
			if _, _, err := sp.PutPage(fmt.Sprintf("%s/p%d", sec, j), fmt.Sprintf("Page %d.%d", i, j), text, Refs{}, Cond{}); err != nil {
				b.Fatal(err)
			}
		}
	}
	return sp
}

func BenchmarkGetPage(b *testing.B) {
	sp := benchSpace(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sp.GetPage(fmt.Sprintf("sec%d/p%d", i%100, i%99)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutPage(b *testing.B) {
	sp := benchSpace(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := sp.PutPage(fmt.Sprintf("sec%d/p%d", i%100, i%99), "T", fmt.Sprintf("# edit %d\n\nfox\n", i), Refs{}, Cond{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTree(b *testing.B) {
	sp := benchSpace(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sp.Tree(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchRare(b *testing.B) {
	sp := benchSpace(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sp.Search(fmt.Sprintf("unique%dx%d", i%100, i%99), 20); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchCommon(b *testing.B) {
	sp := benchSpace(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sp.Search("quick fox", 20); err != nil {
			b.Fatal(err)
		}
	}
}
