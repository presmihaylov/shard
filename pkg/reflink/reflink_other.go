//go:build !linux

package reflink

func probe(string) (Filesystem, error) {
	return Filesystem{}, ErrNotLinux
}

func clone(string, string) error {
	return ErrNotLinux
}
