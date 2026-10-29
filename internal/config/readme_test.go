package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The README lists every setting; a new flag without documentation fails here.
func TestReadmeMentionsEveryFlag(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skip("no README next to the sources")
	}
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	flags := regexp.MustCompile(`fs\.\w+Var\(&c\.\w+, "([a-z-]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(flags) < 20 {
		t.Fatalf("only %d flags found; has the registration style changed?", len(flags))
	}
	for _, m := range flags {
		if !strings.Contains(string(readme), "`-"+m[1]+"`") {
			t.Errorf("README.md does not document the flag -%s", m[1])
		}
	}
}
