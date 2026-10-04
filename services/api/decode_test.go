package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// A route that decodes its own body answers the limit and the fix past the cap, as a typed route does.
func TestDecodePastTheCapNamesTheLimit(t *testing.T) {
	body := strings.NewReader(`{"ref":"` + strings.Repeat("a", maxBody) + `"}`)
	var req pullRequest
	err := decode(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v0/images/pull", body), &req)

	if status, code := classify(err); status != http.StatusRequestEntityTooLarge || code != models.CodeBodyTooLarge {
		t.Errorf("a body past the cap is %d %s, want 413 body_too_large", status, code)
	}
	if text, ok := sandbox.PublicText(err); !ok || text != "the request body exceeds 1 MiB; send a smaller JSON body" {
		t.Errorf("a body past the cap reads %q, want the limit and the fix", text)
	}
}
