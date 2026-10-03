package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/image"
)

const pulledDigest = "sha256:1a2b3c4d5e6f00000000000000000000000000000000000000000000000000aa"

func pullOfTwoLayers() []image.Event {
	return []image.Event{
		{Status: image.StatusPulling, Reference: "docker.io/library/alpine:3.20", Digest: pulledDigest, Layers: 2, Bytes: 3_600_000},
		{Status: image.StatusLayer, Digest: "sha256:9f8e7d6c5b4a99999999", Bytes: 3_500_000},
		{Status: image.StatusLayer, Digest: "sha256:0123456789ab88888888", Bytes: 100_000, Present: true},
		{Status: image.StatusUnpacking, Reference: "docker.io/library/alpine:3.20", Digest: pulledDigest, Layers: 2},
		{Status: image.StatusUnpacked, Digest: "sha256:9f8e7d6c5b4a99999999", Layer: 1, Layers: 2},
		{Status: image.StatusUnpacked, Digest: "sha256:0123456789ab88888888", Layer: 2, Layers: 2},
		{Status: image.StatusPulled, Reference: "docker.io/library/alpine:3.20", Digest: pulledDigest, Path: "/var/lib/shard/images/alpine/rootfs"},
	}
}

const pullOfTwoLayersText = `pulling docker.io/library/alpine:3.20 ` + pulledDigest + `, 2 layers, 3.6 MB
  9f8e7d6c5b4a  3.5 MB
  0123456789ab  100.0 kB, already on disk
unpacking docker.io/library/alpine:3.20, 2 layers
  9f8e7d6c5b4a  unpacked, 1 of 2
  0123456789ab  unpacked, 2 of 2
pulled docker.io/library/alpine:3.20 into /var/lib/shard/images/alpine/rootfs
`

func TestPullLineSaysEachStep(t *testing.T) {
	cases := []struct {
		event client.PullEvent
		want  string
	}{
		{client.PullEvent{Status: client.PullCached, Reference: "alpine:3.20", Digest: "sha256:beef", Path: "/images/alpine"}, "alpine:3.20 sha256:beef is already on disk at /images/alpine"},
		{client.PullEvent{Status: client.PullPulling, Reference: "alpine:3.20", Digest: "sha256:beef", Layers: 1, Bytes: 999}, "pulling alpine:3.20 sha256:beef, 1 layer, 999 B"},
		{client.PullEvent{Status: client.PullUnpacking, Reference: "alpine:3.20", Layers: 1}, "unpacking alpine:3.20, 1 layer"},
		{client.PullEvent{Status: client.PullBuilding, Path: "/images/disks/sha256-beef.ext4"}, "  building /images/disks/sha256-beef.ext4"},
		{client.PullEvent{Status: "verifying"}, "pull: verifying"},
	}

	for _, c := range cases {
		if got := pullLine(c.event); got != c.want {
			t.Errorf("pullLine(%+v) = %q, want %q", c.event, got, c.want)
		}
	}
}

// The progress is for the operator, so id=$(shard create alpine) still takes the id alone.
func TestCreatePrintsThePullOnStderrAndTheIDAloneOnStdout(t *testing.T) {
	var out, errOut bytes.Buffer

	app, d, r := newDaemonCreateApp(t, &out)
	app.Err = &errOut
	d.imageSvc = fakeImages{r: r, pulled: pullOfTwoLayers()}

	if err := app.Run(t.Context(), []string{"create", "alpine:3.20"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if out.String() != "sandbox2\n" {
		t.Errorf("create printed %q on stdout, want the bare id", out.String())
	}
	if errOut.String() != pullOfTwoLayersText {
		t.Errorf("create printed\n%s\non stderr, want\n%s", errOut.String(), pullOfTwoLayersText)
	}
}

func TestPullPrintsTheSameStepsAsCreate(t *testing.T) {
	var out, errOut bytes.Buffer

	app, d, r := newDaemonCreateApp(t, &out)
	app.Err = &errOut
	d.imageSvc = fakeImages{r: r, pulled: pullOfTwoLayers()}

	if err := app.Run(t.Context(), []string{"pull", "alpine:3.20"}); err != nil {
		t.Fatalf("pull: %v", err)
	}

	if errOut.String() != pullOfTwoLayersText {
		t.Errorf("pull printed\n%s\non stderr, want\n%s", errOut.String(), pullOfTwoLayersText)
	}
	if strings.Contains(out.String(), "pulling") {
		t.Errorf("pull printed its progress on stdout: %q", out.String())
	}
}
