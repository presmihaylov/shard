package image

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/presmihaylov/shard/pkg/ext4"
)

const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

var errMergeStopped = errors.New("the layer merge stopped")

// version names one tar entry by the layer it sits in and its place in that layer.
type version struct {
	layer, seq int
}

// merge is the final tree once the whiteouts are applied, plus the older versions the hard links took.
type merge struct {
	tree   map[string]version
	links  map[version]version
	needed map[version]bool
	// names holds the name of every version a link took, and home where the ones that lost it are written: the first link.
	names map[version]string
	home  map[version]string
}

// buildDisk writes the layers as one ext4 image at dst, from the tars: an unpack on a Mac loses the uid, the devices and the xattrs.
func buildDisk(ctx context.Context, dst string, layers []v1.Layer) (err error) {
	m, err := planDisk(ctx, layers)
	if err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", dst, cerr))
		}
	}()

	// The survivors stream as one tar into the image writer, which stops on the sentinel when the merge fails.
	pr, pw := io.Pipe()
	merged := make(chan error, 1)
	go func() {
		err := m.writeTar(ctx, pw, layers)
		if err != nil {
			merged <- errors.Join(err, pw.CloseWithError(errMergeStopped))

			return
		}
		merged <- pw.Close()
	}()
	werr := ext4.Write(pr, f)
	if werr == nil || errors.Is(werr, errMergeStopped) {
		return <-merged
	}
	// The writer failed on its own; what the merge says after this close is only that the pipe is gone.
	if err := pr.CloseWithError(werr); err != nil {
		return errors.Join(werr, err)
	}
	<-merged

	return fmt.Errorf("write %s: %w", dst, werr)
}

// writeTar lays the survivors down over a second pass of the layers, in the order the layers hold them.
func (m *merge) writeTar(ctx context.Context, w io.Writer, layers []v1.Layer) error {
	tw := tar.NewWriter(w)
	for i, layer := range layers {
		if err := walkLayer(ctx, layer, func(seq int, hdr *tar.Header, body io.Reader) error {
			return m.write(tw, version{i, seq}, hdr, body)
		}); err != nil {
			return fmt.Errorf("write layer %d: %w", i, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish the tar: %w", err)
	}

	return nil
}

// planDisk reads every layer once for its headers and settles which version of each path survives.
func planDisk(ctx context.Context, layers []v1.Layer) (*merge, error) {
	m := &merge{tree: map[string]version{}, links: map[version]version{}, needed: map[version]bool{}, names: map[version]string{}, home: map[version]string{}}
	for i, layer := range layers {
		if err := walkLayer(ctx, layer, func(seq int, hdr *tar.Header, _ io.Reader) error {
			return m.plan(version{i, seq}, hdr)
		}); err != nil {
			return nil, fmt.Errorf("plan layer %d: %w", i, err)
		}
	}

	for name, v := range m.tree {
		m.needed[v] = true
		tv, ok := m.links[v]
		if !ok {
			continue
		}
		m.needed[tv] = true
		// A target that lost its name lives on through its links alone; the earliest one holds the body.
		if m.tree[m.names[tv]] == tv {
			continue
		}
		if held, ok := m.home[tv]; !ok || v.before(m.tree[held]) {
			m.home[tv] = name
		}
	}

	return m, nil
}

func (v version) before(o version) bool {
	return v.layer < o.layer || v.layer == o.layer && v.seq < o.seq
}

func (m *merge) plan(v version, hdr *tar.Header) error {
	name, ok := cleanName(hdr.Name)
	if !ok {
		return nil
	}

	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if base == opaqueWhiteout {
		m.dropBelow(dir, v.layer)

		return nil
	}
	if gone, ok := strings.CutPrefix(base, whiteoutPrefix); ok {
		m.dropBelow(path.Join(dir, gone), v.layer)

		return nil
	}

	// A hard link takes the target as it is now, even if a later layer replaces or removes the target's name.
	if hdr.Typeflag == tar.TypeLink {
		target, ok := cleanName(hdr.Linkname)
		if !ok {
			return fmt.Errorf("%s: a hard link to the root", name)
		}
		tv, ok := m.tree[target]
		if !ok {
			return fmt.Errorf("%s: a hard link to %s, which the layers so far do not hold", name, target)
		}
		m.names[tv] = target
		// A link to a link takes the file behind both, so a whiteout of the middle name changes nothing.
		if ftv, ok := m.links[tv]; ok {
			tv = ftv
		}
		m.links[v] = tv
	}

	// A directory entry only refreshes its metadata; the files under it from lower layers stay.
	if hdr.Typeflag == tar.TypeDir {
		delete(m.tree, name)
	}
	if hdr.Typeflag != tar.TypeDir {
		m.dropBelow(name, v.layer+1)
	}
	m.tree[name] = v

	return nil
}

// dropBelow forgets name and everything under it that a layer before limit wrote.
func (m *merge) dropBelow(name string, limit int) {
	for p, v := range m.tree {
		if v.layer < limit && (p == name || name == "" || strings.HasPrefix(p, name+"/")) {
			delete(m.tree, p)
		}
	}
}

// write puts one tar entry on the stream, when the plan needs that version.
func (m *merge) write(tw *tar.Writer, v version, hdr *tar.Header, body io.Reader) error {
	name, ok := cleanName(hdr.Name)
	if !ok || !m.needed[v] {
		return nil
	}
	if err := checkType(hdr); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	out := *hdr
	out.Name = name
	if hdr.Typeflag == tar.TypeLink {
		target := m.links[v]
		if home, ok := m.home[target]; ok && home == name {
			// This link holds the body, which went out under this name when its version passed.
			return nil
		}
		out.Linkname = m.linkName(target)
	}
	// A version a link took but a later layer replaced or removed goes out under the link's name.
	if m.tree[name] != v {
		out.Name = m.home[v]
	}
	if err := tw.WriteHeader(&out); err != nil {
		return fmt.Errorf("%s: %w", out.Name, err)
	}
	if hdr.Typeflag == tar.TypeReg {
		if _, err := io.CopyN(tw, body, hdr.Size); err != nil {
			return fmt.Errorf("%s: %w", out.Name, err)
		}
	}

	return nil
}

// linkName is where the version a link took sits on the disk: its own name, or the home of the link that kept it.
func (m *merge) linkName(target version) string {
	if home, ok := m.home[target]; ok {
		return home
	}

	return m.names[target]
}

// readerOf ends a tar at the next read after ctx ends, so a large body cannot outlive a cancel in either pass.
func readerOf(ctx context.Context, r io.Reader) io.Reader {
	return readerFunc(func(p []byte) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		return r.Read(p)
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// checkType refuses what the guest cannot hold, which the image writer would otherwise lay down as a plain file.
func checkType(hdr *tar.Header) error {
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return nil
	}

	return fmt.Errorf("tar type %q is not a file the guest can hold", hdr.Typeflag)
}

// cleanName is the path inside the image, with no leading slash; the root itself is not an entry.
func cleanName(name string) (string, bool) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")

	return name, name != ""
}

// walkLayer streams one layer's tar and hands each header, with its body, to fn.
func walkLayer(ctx context.Context, layer v1.Layer, fn func(seq int, hdr *tar.Header, body io.Reader) error) (err error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("open the layer: %w", err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close the layer: %w", cerr))
		}
	}()

	tr := tar.NewReader(readerOf(ctx, rc))
	for seq := 0; ; seq++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the tar: %w", err)
		}
		if err := fn(seq, hdr, tr); err != nil {
			return err
		}
	}
}
