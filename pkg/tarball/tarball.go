// Package tarball packs a tree into a tar stream and unpacks one under a directory it never writes outside of.
package tarball

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Pack writes src as a tar stream whose top entry is name: a directory with everything under it in lexical order, or one entry for anything else.
func Pack(w io.Writer, src, name string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		// A name removed between the directory read and its visit is gone, which is what a later walk would say too.
		if errors.Is(err, fs.ErrNotExist) && p != src {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}

		return addEntry(tw, p, path.Join(name, filepath.ToSlash(rel)), d)
	})
	if err != nil {
		return err
	}

	return tw.Close()
}

// addEntry writes one entry: a socket has no tar type and stays out, and a fifo or a device goes in as a header alone.
func addEntry(tw *tar.Writer, p, name string, d fs.DirEntry) error {
	info, err := d.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Type() == fs.ModeSocket {
		return nil
	}

	link := ""
	if info.Mode().Type() == fs.ModeSymlink {
		if link, err = os.Readlink(p); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("describe %s: %w", p, err)
	}
	hdr.Name = name
	if info.IsDir() {
		hdr.Name += "/"
	}
	if !info.Mode().IsRegular() {
		return writeHeader(tw, hdr)
	}

	return addFile(tw, p, hdr)
}

// addFile copies exactly the size the header names from a file opened without following a link, so a file that changes under the walk fails and never lies.
func addFile(tw *tar.Writer, p string, hdr *tar.Header) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	if err := writeHeader(tw, hdr); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
		return errors.Join(fmt.Errorf("copy %s into the archive: %w", p, err), f.Close())
	}

	return f.Close()
}

func writeHeader(tw *tar.Writer, hdr *tar.Header) error {
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("pack %s: %w", hdr.Name, err)
	}

	return nil
}

// Options says what one unpack takes. The host's unpack of a guest's tar confines links and caps the stream, since that tar is untrusted.
type Options struct {
	// Strip is the name every entry must be or sit under; it is cut, so the entry named Strip lands at dst itself.
	Strip string
	// KeepSetid keeps setuid and setgid, which only an unpack inside the sandbox does.
	KeepSetid bool
	// ConfineLinks refuses a symlink that resolves outside dst, alone or through another link.
	ConfineLinks bool
	// MaxBytes caps the bytes of every file together, and MaxEntries the entries; zero leaves that one unbounded.
	MaxBytes   int64
	MaxEntries int
	// Owner, when set, takes every entry this unpack makes; one already there keeps its own.
	Owner *Owner
}

// Owner is who a host unpack hands what it makes to, as a copy out under sudo does for the user who ran it.
type Owner struct {
	UID int
	GID int
}

// RefusedError names the entry an unpack refused and why; the unpack writes nothing after it.
type RefusedError struct {
	Name   string
	Reason string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("refuse the entry %q: %s", e.Name, e.Reason)
}

// Unpack writes the tar under dst and never outside it. An existing directory keeps its own mode and owner, and only an Owner chowns.
func Unpack(r io.Reader, dst string, opts Options) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	// A root refuses an absolute name with the os package's own escape error, before any lookup; it does not export it.
	_, lookup := root.Lstat("/")
	escape := errors.Unwrap(lookup)
	if escape == nil {
		return errors.Join(fmt.Errorf("a root answered an absolute name with no escape error of its own: %w", lookup), root.Close())
	}
	u := &unpacker{root: root, opts: opts, escape: escape, files: map[string]fs.FileInfo{}}

	return errors.Join(u.run(tar.NewReader(r)), root.Close())
}

type unpacker struct {
	root   *os.Root
	opts   Options
	escape error
	bytes  int64
	temps  int
	// files are the regular files this unpack wrote, by identity, the only targets a hard link may name.
	files map[string]fs.FileInfo
	dirs  []madeDir
	links []madeLink
}

type madeDir struct {
	name string
	mode fs.FileMode
}

type madeLink struct {
	name  string
	entry string
}

func (u *unpacker) run(tr *tar.Reader) error {
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return u.finish()
		}
		if err != nil {
			return errors.Join(fmt.Errorf("read the tar: %w", err), u.confine())
		}
		if u.opts.MaxEntries > 0 && entries >= u.opts.MaxEntries {
			return errors.Join(&RefusedError{Name: hdr.Name, Reason: fmt.Sprintf("the archive runs past %d entries", u.opts.MaxEntries)}, u.confine())
		}
		if err := u.entry(tr, hdr); err != nil {
			return errors.Join(err, u.confine())
		}
	}
}

func (u *unpacker) entry(tr *tar.Reader, hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	name, err := u.local(hdr.Name)
	if err != nil {
		return err
	}
	if name == "." && hdr.Typeflag != tar.TypeDir {
		return &RefusedError{Name: hdr.Name, Reason: "only a directory can land at the destination itself"}
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return u.dir(name, hdr)
	case tar.TypeReg:
		return u.file(name, hdr, tr)
	case tar.TypeSymlink:
		return u.symlink(name, hdr)
	case tar.TypeLink:
		return u.hardlink(name, hdr)
	case tar.TypeChar, tar.TypeBlock:
		return &RefusedError{Name: hdr.Name, Reason: "a device node"}
	case tar.TypeFifo:
		return &RefusedError{Name: hdr.Name, Reason: "a fifo, which an unpack does not make"}
	default:
		return &RefusedError{Name: hdr.Name, Reason: fmt.Sprintf("the type %q, which an unpack does not make", hdr.Typeflag)}
	}
}

// local turns an entry name into a path under dst: it refuses an absolute name and any .. before the clean, then cuts Strip.
func (u *unpacker) local(name string) (string, error) {
	if name == "" {
		return "", &RefusedError{Name: name, Reason: "an empty name"}
	}
	if strings.HasPrefix(name, "/") {
		return "", &RefusedError{Name: name, Reason: "an absolute name"}
	}
	if slices.Contains(strings.Split(name, "/"), "..") {
		return "", &RefusedError{Name: name, Reason: "a .. component"}
	}

	clean := path.Clean(name)
	if u.opts.Strip == "" {
		return clean, nil
	}
	if clean == u.opts.Strip {
		return ".", nil
	}
	rest, ok := strings.CutPrefix(clean, u.opts.Strip+"/")
	if !ok {
		return "", &RefusedError{Name: name, Reason: "it is not under " + u.opts.Strip}
	}

	return rest, nil
}

// dir makes a directory only its owner can write, so its entries land whatever its mode, and sets the tar's mode once the tree is in.
func (u *unpacker) dir(name string, hdr *tar.Header) error {
	if err := u.parent(name, hdr.Name); err != nil {
		return err
	}

	err := u.root.Mkdir(name, 0o700)
	if errors.Is(err, fs.ErrExist) {
		info, err := u.root.Stat(name)
		if err != nil {
			return u.refusal(hdr.Name, err)
		}
		if !info.IsDir() {
			return &RefusedError{Name: hdr.Name, Reason: "a directory where something else already is"}
		}

		return nil
	}
	if err != nil {
		return u.refusal(hdr.Name, err)
	}
	if err := u.own(name, hdr.Name); err != nil {
		return err
	}
	u.dirs = append(u.dirs, madeDir{name: name, mode: u.mode(hdr)})

	return nil
}

// file lands the bytes under a temp name beside the target and renames it over, so a link already at the name is replaced and never followed.
func (u *unpacker) file(name string, hdr *tar.Header, r io.Reader) error {
	if u.opts.MaxBytes > 0 && hdr.Size > u.opts.MaxBytes-u.bytes {
		return &RefusedError{Name: hdr.Name, Reason: fmt.Sprintf("the archive runs past %d bytes", u.opts.MaxBytes)}
	}
	u.bytes += hdr.Size
	if err := u.parent(name, hdr.Name); err != nil {
		return err
	}

	tmp := u.tempName(name)
	f, err := u.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return u.refusal(hdr.Name, err)
	}
	// The chown goes before the chmod, as a chown clears setuid and setgid.
	if err := u.own(tmp, hdr.Name); err != nil {
		return errors.Join(err, f.Close(), u.removeTemp(tmp))
	}
	if err := fill(f, r, hdr.Size, u.mode(hdr)); err != nil {
		return errors.Join(fmt.Errorf("unpack %s: %w", hdr.Name, err), u.removeTemp(tmp))
	}

	return u.place(tmp, name, hdr.Name, true)
}

func fill(f *os.File, r io.Reader, size int64, mode fs.FileMode) error {
	if _, err := io.CopyN(f, r, size); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Chmod(mode); err != nil {
		return errors.Join(err, f.Close())
	}

	return f.Close()
}

func (u *unpacker) symlink(name string, hdr *tar.Header) error {
	if u.opts.ConfineLinks && leaves(name, hdr.Linkname) {
		return &RefusedError{Name: hdr.Name, Reason: fmt.Sprintf("a symlink to %q, which leaves the destination", hdr.Linkname)}
	}
	if err := u.parent(name, hdr.Name); err != nil {
		return err
	}

	tmp := u.tempName(name)
	if err := u.root.Symlink(hdr.Linkname, tmp); err != nil {
		return u.refusal(hdr.Name, err)
	}
	if err := u.own(tmp, hdr.Name); err != nil {
		return errors.Join(err, u.removeTemp(tmp))
	}
	if u.opts.ConfineLinks {
		u.links = append(u.links, madeLink{name: name, entry: hdr.Name})
	}

	return u.place(tmp, name, hdr.Name, false)
}

// leaves is the lexical half of the link check: an absolute target, or a .. that climbs past dst from where the link sits.
func leaves(name, target string) bool {
	if path.IsAbs(target) {
		return true
	}
	resolved := path.Join(path.Dir(name), target)

	return resolved == ".." || strings.HasPrefix(resolved, "../")
}

// hardlink links to a file this unpack already wrote, and to nothing else, so a guest cannot link the host's own files into dst.
func (u *unpacker) hardlink(name string, hdr *tar.Header) error {
	target, err := u.local(hdr.Linkname)
	wrote, ok := u.files[target]
	if err != nil || !ok {
		return &RefusedError{Name: hdr.Name, Reason: fmt.Sprintf("a hard link to %q, which is not a file earlier in the archive", hdr.Linkname)}
	}
	if err := u.parent(name, hdr.Name); err != nil {
		return err
	}

	tmp := u.tempName(name)
	if err := u.root.Link(target, tmp); err != nil {
		return u.refusal(hdr.Name, err)
	}
	// A filesystem that folds case lets a later entry spelled another way take the name, so the link must be the very file written there.
	linked, err := u.root.Lstat(tmp)
	if err != nil {
		return errors.Join(u.refusal(hdr.Name, err), u.removeTemp(tmp))
	}
	if !linked.Mode().IsRegular() || !os.SameFile(wrote, linked) {
		reason := fmt.Sprintf("a hard link to %q, which no longer holds the file the archive wrote there", hdr.Linkname)
		return errors.Join(&RefusedError{Name: hdr.Name, Reason: reason}, u.removeTemp(tmp))
	}

	return u.place(tmp, name, hdr.Name, true)
}

// place renames a finished temp over the name, and records whether a hard link may name it now.
func (u *unpacker) place(tmp, name, entry string, file bool) error {
	if err := u.root.Rename(tmp, name); err != nil {
		return errors.Join(u.refusal(entry, err), u.removeTemp(tmp))
	}
	if !file {
		delete(u.files, name)

		return nil
	}
	info, err := u.root.Lstat(name)
	if err != nil {
		return u.refusal(entry, err)
	}
	u.files[name] = info

	return nil
}

// parent makes each missing directory above name as mkdir -p does, one at a time so the owner takes each one it makes.
func (u *unpacker) parent(name, entry string) error {
	dir := path.Dir(name)
	if dir == "." {
		return nil
	}
	err := u.root.Mkdir(dir, 0o755) //nolint:gosec // G301: a parent reads as mkdir -p makes it, as tar does
	if errors.Is(err, fs.ErrNotExist) {
		if err := u.parent(dir, entry); err != nil {
			return err
		}
		err = u.root.Mkdir(dir, 0o755) //nolint:gosec // G301: as above
	}
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return u.refusal(entry, err)
	}

	return u.own(dir, entry)
}

// own hands a name this unpack made to the Owner, when there is one.
func (u *unpacker) own(name, entry string) error {
	if u.opts.Owner == nil {
		return nil
	}
	if err := u.root.Lchown(name, u.opts.Owner.UID, u.opts.Owner.GID); err != nil {
		return u.refusal(entry, err)
	}

	return nil
}

func (u *unpacker) tempName(name string) string {
	u.temps++

	return path.Join(path.Dir(name), fmt.Sprintf(".shard-unpack-%d-%d", os.Getpid(), u.temps))
}

func (u *unpacker) removeTemp(tmp string) error {
	if err := u.root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// mode is the tar's mode as fs bits; the host drops setuid and setgid, so a guest cannot hand it a set-id binary.
func (u *unpacker) mode(hdr *tar.Header) fs.FileMode {
	keep := fs.ModePerm | fs.ModeSticky
	if u.opts.KeepSetid {
		keep |= fs.ModeSetuid | fs.ModeSetgid
	}

	return hdr.FileInfo().Mode() & keep
}

// refusal names the entry a write failed on, and turns the escape error into a refusal: only a link in the archive or in dst causes it.
func (u *unpacker) refusal(entry string, err error) error {
	if errors.Is(err, u.escape) {
		return &RefusedError{Name: entry, Reason: "it leaves the destination through a symlink"}
	}

	return fmt.Errorf("unpack %s: %w", entry, err)
}

// finish confines the new links, then sets each new directory's mode, deepest first, so its entries landed whatever that mode is.
func (u *unpacker) finish() error {
	if err := u.confine(); err != nil {
		return err
	}

	for _, dir := range slices.Backward(u.dirs) {
		if err := u.root.Chmod(dir.name, dir.mode); err != nil {
			return fmt.Errorf("set the mode of %s: %w", dir.name, err)
		}
	}

	return nil
}

// confine removes every new link that leaves dst through another one, which only the tree as written shows, so a refused unpack leaves none behind either.
func (u *unpacker) confine() error {
	var refused error
	for _, link := range u.links {
		_, err := u.root.Stat(link.name)
		if errors.Is(err, u.escape) {
			refused = errors.Join(refused, &RefusedError{Name: link.entry, Reason: "a symlink that leaves the destination through another link"}, u.root.Remove(link.name))

			continue
		}
		// A dangling link, a loop, and a path through a file all stay inside dst.
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP) {
			refused = errors.Join(refused, fmt.Errorf("check the symlink %s: %w", link.entry, err))
		}
	}
	u.links = nil

	return refused
}
