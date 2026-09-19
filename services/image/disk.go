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
	xattrPrefix    = "SCHILY.xattr."
)

// version names one tar entry by the layer it sits in and its place in that layer.
type version struct {
	layer, seq int
}

// merge is the final tree once the whiteouts are applied, plus the older versions the hard links took.
type merge struct {
	tree   map[string]version
	needed map[version]bool
}

// buildDisk writes the layers as one ext4 image at dst, from the tars: an unpack on a Mac loses the uid, the devices and the xattrs.
func buildDisk(ctx context.Context, dst string, layers []v1.Layer) (err error) {
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", dst, cerr)
		}
	}()

	w := ext4.NewWriter(f)
	if err := writeDisk(ctx, w, layers); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish %s: %w", dst, err)
	}

	return nil
}

// writeDisk plans the merge over one pass of the layers and lays the survivors down over a second.
func writeDisk(ctx context.Context, w *ext4.Writer, layers []v1.Layer) error {
	m, err := planDisk(ctx, layers)
	if err != nil {
		return err
	}

	made := map[string]bool{}
	for i, layer := range layers {
		if err := walkLayer(ctx, layer, func(seq int, hdr *tar.Header, body io.Reader) error {
			return m.write(w, made, version{i, seq}, hdr, body)
		}); err != nil {
			return fmt.Errorf("write layer %d: %w", i, err)
		}
	}

	return nil
}

// planDisk reads every layer once for its headers and settles which version of each path survives.
func planDisk(ctx context.Context, layers []v1.Layer) (*merge, error) {
	m := &merge{tree: map[string]version{}, needed: map[version]bool{}}
	for i, layer := range layers {
		if err := walkLayer(ctx, layer, func(seq int, hdr *tar.Header, _ io.Reader) error {
			return m.plan(version{i, seq}, hdr)
		}); err != nil {
			return nil, fmt.Errorf("plan layer %d: %w", i, err)
		}
	}

	for _, v := range m.tree {
		m.needed[v] = true
	}

	return m, nil
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

	// A hard link takes the target as it is now, even if a later layer replaces the target's name.
	if hdr.Typeflag == tar.TypeLink {
		target, ok := cleanName(hdr.Linkname)
		if !ok {
			return fmt.Errorf("%s: a hard link to the root", name)
		}
		tv, ok := m.tree[target]
		if !ok {
			return fmt.Errorf("%s: a hard link to %s, which the layers so far do not hold", name, target)
		}
		m.needed[tv] = true
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

// write lays down one tar entry, when the plan needs that version, after the directories above it.
func (m *merge) write(w *ext4.Writer, made map[string]bool, v version, hdr *tar.Header, body io.Reader) error {
	name, ok := cleanName(hdr.Name)
	if !ok || !m.needed[v] {
		return nil
	}
	if err := m.parents(w, made, name); err != nil {
		return err
	}

	if hdr.Typeflag == tar.TypeLink {
		target, _ := cleanName(hdr.Linkname)
		if err := w.Link(target, name); err != nil {
			return err
		}
		made[name] = true

		return nil
	}

	f, err := fileOf(hdr)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := w.Create(name, f); err != nil {
		return err
	}
	if hdr.Typeflag == tar.TypeReg {
		if _, err := io.CopyN(w, body, hdr.Size); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	made[name] = true

	return nil
}

// parents makes every directory above name that is not made yet, from the tar when the layers hold it.
func (m *merge) parents(w *ext4.Writer, made map[string]bool, name string) error {
	dir := path.Dir(name)
	if dir == "." || made[dir] {
		return nil
	}
	if err := m.parents(w, made, dir); err != nil {
		return err
	}

	f := &ext4.File{Mode: ext4.S_IFDIR | 0o755}
	if err := w.Create(dir, f); err != nil {
		return err
	}
	made[dir] = true

	return nil
}

func fileOf(hdr *tar.Header) (*ext4.File, error) {
	var typ uint16
	switch hdr.Typeflag {
	case tar.TypeReg:
		typ = ext4.S_IFREG
	case tar.TypeDir:
		typ = ext4.S_IFDIR
	case tar.TypeSymlink:
		typ = ext4.S_IFLNK
	case tar.TypeChar:
		typ = ext4.S_IFCHR
	case tar.TypeBlock:
		typ = ext4.S_IFBLK
	case tar.TypeFifo:
		typ = ext4.S_IFIFO
	default:
		return nil, fmt.Errorf("tar type %q is not a file the guest can hold", hdr.Typeflag)
	}

	f := &ext4.File{
		Mode:     uint16(hdr.Mode)&^ext4.TypeMask | typ, //nolint:gosec // the mode is twelve bits
		Uid:      uint32(hdr.Uid),                       //nolint:gosec // an id is 32 bits on the guest
		Gid:      uint32(hdr.Gid),                       //nolint:gosec // an id is 32 bits on the guest
		Size:     hdr.Size,
		Atime:    hdr.AccessTime,
		Mtime:    hdr.ModTime,
		Ctime:    hdr.ChangeTime,
		Crtime:   hdr.ModTime,
		Linkname: hdr.Linkname,
		Devmajor: uint32(hdr.Devmajor), //nolint:gosec // a device number is 32 bits on the guest
		Devminor: uint32(hdr.Devminor), //nolint:gosec // a device number is 32 bits on the guest
		Xattrs:   map[string][]byte{},
	}
	for key, value := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(key, xattrPrefix); ok {
			f.Xattrs[name] = []byte(value)
		}
	}

	return f, nil
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
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close the layer: %w", cerr)
		}
	}()

	tr := tar.NewReader(rc)
	for seq := 0; ; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
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
