package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestInspectPrintsTheRecordAsJSON(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

	if err := app.Run(t.Context(), []string{"inspect", "web"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	var got models.Sandbox
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("inspect printed something that is not JSON: %v\n%s", err, out.String())
	}
	if got.ID != "sandbox1" || got.State != models.StateStopped || got.ExitStatus == nil || got.ExitStatus.Code != 3 {
		t.Errorf("inspect printed %+v", got)
	}
	if !strings.Contains(out.String(), `"state": "stopped"`) {
		t.Errorf("the state is not a top-level field jq can read:\n%s", out.String())
	}
}

func TestInspectNamesAMissingSandbox(t *testing.T) {
	var out bytes.Buffer

	app, d := newLifecycleApp(t, &out, &recorder{}, models.Sandbox{})
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"inspect", "ghost"})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("inspect returned %v, want the missing sandbox named", err)
	}
}
