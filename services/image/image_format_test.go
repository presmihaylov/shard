package image_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/services/image"
)

// formats are the files a VM provider boots from, which a container provider never built for the tags it pulled.
var formats = []struct {
	name string
	opt  image.Option
	path func(image.Image) string
}{
	{name: "disk", opt: image.WithDisks(), path: func(img image.Image) string { return img.Disk }},
	{name: "erofs", opt: image.WithErofs(), path: func(img image.Image) string { return img.Erofs }},
}

func TestANewFormatKeepsTheDigestTheTagHeld(t *testing.T) {
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			fakeMkfsErofs(t)
			server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "first"})
			root := t.TempDir()
			held, err := newServiceAt(t, root, server).Pull(t.Context(), ref)
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}
			pushImage(t, server, "app:1.0", map[string]string{"etc/hostname": "second"})

			progress := image.NewProgress()
			img, err := newServiceAt(t, root, server, format.opt).Pull(image.WithProgress(t.Context(), progress), ref)
			if err != nil {
				t.Fatalf("Pull with the %s: %v", format.name, err)
			}
			progress.Close()
			if img.Digest != held.Digest {
				t.Errorf("the %s pull resolved the tag again: digest %s, want the held %s", format.name, img.Digest, held.Digest)
			}
			hostname, err := os.ReadFile(filepath.Join(img.RootFS, "etc", "hostname"))
			if err != nil || string(hostname) != "first" {
				t.Errorf("the tree holds %q (%v), want the held first", hostname, err)
			}
			if _, err := os.Stat(format.path(img)); err != nil {
				t.Errorf("the %s is not there: %v", format.name, err)
			}
			want := []string{image.StatusBuilding, image.StatusPulled}
			if got := statuses(events(t, progress)); !slices.Equal(got, want) {
				t.Errorf("the %s pull said %v, want %v", format.name, got, want)
			}
		})
	}
}

func TestANewFormatBuildsWithTheRegistryGone(t *testing.T) {
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			fakeMkfsErofs(t)
			server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
			root := t.TempDir()
			held, err := newServiceAt(t, root, server).Pull(t.Context(), ref)
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}
			server.Close()

			img, err := newServiceAt(t, root, server, format.opt).Pull(t.Context(), ref)
			if err != nil {
				t.Fatalf("Pull with the %s and no registry: %v", format.name, err)
			}
			if img.Digest != held.Digest {
				t.Errorf("digest %s, want the held %s", img.Digest, held.Digest)
			}
			if _, err := os.Stat(format.path(img)); err != nil {
				t.Errorf("the %s is not there: %v", format.name, err)
			}
		})
	}
}

func TestAFailedBuildOfANewFormatKeepsTheHeldTag(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	held, err := newServiceAt(t, root, server).Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	failingMkfsErofs(t)

	svc := newServiceAt(t, root, server, image.WithErofs())
	if _, err := svc.Pull(t.Context(), ref); err == nil {
		t.Fatal("Pull with a failing mkfs.erofs returned no error")
	}

	images, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 1 || images[0].Digest != held.Digest {
		t.Errorf("the store lists %+v, want the held %s", images, held.Digest)
	}
	if _, err := os.Stat(held.RootFS); err != nil {
		t.Errorf("the held tree went: %v", err)
	}
}
