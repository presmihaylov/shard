package client_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// stream answers the lines as ndjson, the way the daemon does once a pull has started, and records what the request accepted.
func stream(accepted *string, lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*accepted = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
	}
}

func collect(events *[]client.PullEvent) func(client.PullEvent) {
	return func(e client.PullEvent) { *events = append(*events, e) }
}

func TestPullImageReportsEachEventThenAnswersTheImage(t *testing.T) {
	var accepted string
	c := serve(t, shortRoot(t), stream(&accepted,
		`{"event":{"status":"pulling","reference":"docker.io/library/alpine:3.20","digest":"sha256:beef","layers":1,"bytes":3}}`,
		`{"event":{"status":"layer","digest":"sha256:cafe","bytes":3}}`,
		`{"event":{"status":"pulled","reference":"docker.io/library/alpine:3.20","path":"/images/alpine"}}`,
		`{"image":{"reference":"docker.io/library/alpine:3.20","digest":"sha256:beef"}}`,
	))

	var events []client.PullEvent
	img, err := c.PullImage(t.Context(), "alpine:3.20", collect(&events))
	if err != nil || img.Digest != "sha256:beef" {
		t.Fatalf("PullImage = %+v, %v; want the image the last line named", img, err)
	}

	if accepted != "application/x-ndjson" {
		t.Errorf("the request accepted %q, want the ndjson stream", accepted)
	}
	got := make([]string, len(events))
	for i, e := range events {
		got[i] = e.Status
	}
	if want := []string{client.PullPulling, client.PullLayer, client.PullPulled}; !slices.Equal(got, want) || events[1].Bytes != 3 {
		t.Errorf("the report heard %+v, want %v with the layer's bytes", events, want)
	}
}

func TestPullImageWithNoReportAsksForTheImageAlone(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		saw.contentType = r.Header.Get("Accept")
		answer(http.StatusOK, `{"reference":"docker.io/library/alpine:3.20","digest":"sha256:beef"}`)(w, r)
	})

	img, err := c.PullImage(t.Context(), "alpine:3.20", nil)
	if err != nil || img.Digest != "sha256:beef" || strings.Contains(saw.contentType, "ndjson") {
		t.Errorf("PullImage = %+v, %v accepting %q; want the image as plain JSON", img, err, saw.contentType)
	}
}

func TestAnErrorLineIsTheDaemonsRefusal(t *testing.T) {
	var accepted string
	c := serve(t, shortRoot(t), stream(&accepted,
		`{"event":{"status":"pulling","reference":"docker.io/library/alpine:3.20"}}`,
		`{"error":{"code":"in_use","message":"image alpine:3.20 is in use","holders":["web"]}}`,
	))

	_, err := c.PullImage(t.Context(), "alpine:3.20", func(client.PullEvent) {})

	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Code != models.CodeInUse || refused.Message != "image alpine:3.20 is in use" || !slices.Equal(refused.Holders, []string{"web"}) {
		t.Errorf("PullImage = %v, want the error line as the daemon's refusal", err)
	}
}

func TestAStreamThatEndsBeforeTheResultIsAnError(t *testing.T) {
	var accepted string
	c := serve(t, shortRoot(t), stream(&accepted, `{"event":{"status":"pulling"}}`))

	_, err := c.CreateSandboxAndWait(t.Context(), sandbox.CreateRequest{Image: "alpine:3.20"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ended the answer before the result") {
		t.Errorf("CreateSandboxAndWait = %v, want the early end named", err)
	}
}

// A daemon that predates the stream ignores the accept header and answers the record whole.
func TestCreateSandboxAndWaitReadsAPlainAnswer(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusCreated, `{"id":"sandbox1","state":"running"}`, &saw))

	heard := 0
	sb, err := c.CreateSandboxAndWait(t.Context(), sandbox.CreateRequest{Image: "alpine:3.20"}, func(client.PullEvent) { heard++ })
	if err != nil || sb.ID != "sandbox1" || sb.State != models.StateRunning || heard != 0 {
		t.Fatalf("CreateSandboxAndWait = %+v, %v after %d events; want sandbox1 running and no events", sb, err, heard)
	}

	if saw.method != http.MethodPost || saw.uri != "/v0/sandboxes?wait=true" {
		t.Errorf("the request was %s %s, want POST /v0/sandboxes?wait=true", saw.method, saw.uri)
	}
}

func TestCreateSandboxAndWaitKeepsTheStatusOfARefusal(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"secret NOPE does not exist"}}`))

	_, err := c.CreateSandboxAndWait(t.Context(), sandbox.CreateRequest{Image: "alpine:3.20"}, nil)

	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest || refused.Code != models.CodeInvalidRequest {
		t.Errorf("CreateSandboxAndWait = %v, want the 400 as the daemon's refusal", err)
	}
}
