package server

import (
	"strings"
	"testing"
)

// A new space is closed to anonymous visitors until someone turns reading on.
func TestNewSpacesAreNotPublicByDefault(t *testing.T) {
	e := newEnv(t)
	js := "application/json"

	if got := expect(t, e.do("POST", "/_api/spaces", `{"slug":"quiet","title":"Quiet"}`, "Content-Type", js), 201); !strings.Contains(got, `"public_read":false`) {
		t.Errorf("a space made without saying so is public: %s", got)
	}
	if got := expect(t, e.do("POST", "/_api/spaces", `{"slug":"open","title":"Open","public_read":true}`, "Content-Type", js), 201); !strings.Contains(got, `"public_read":true`) {
		t.Errorf("asking for a public space did not make one: %s", got)
	}

	// The "new space" form starts with the box off.
	form := expect(t, e.do("GET", "/_ui/new-space", "", "Accept", "text/html"), 200)
	i := strings.Index(form, `name="public"`)
	if i < 0 || strings.Contains(form[i:i+40], "checked") {
		t.Errorf("the new space form starts with Anyone may read on: %.200s", form[max(i, 0):])
	}
}
