package disk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

type Root struct {
	root *os.Root
}

func New(dir string) (*Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Root{root: root}, nil
}

var (
	ErrInvalidPath = errors.New("invalid path")
	ErrNotEmpty    = errors.New("directory not empty")
)

func (r *Root) Close() error {
	return r.root.Close()

}

func Clean(o string) string {
	return path.Clean("/" + o)
}

func validate(p string) error {
	if len(p) > 4096 {
		return ErrInvalidPath
	}
	if strings.ContainsRune(p, 0) {
		return ErrInvalidPath
	}
	return nil
}

func rel(p string) (string, error) {
	if err := validate(p); err != nil {
		return "", err
	}
	p = Clean(p)
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "."
	}
	return p, nil
}

func relMut(p string) (string, error) {
	relativePath, err := rel(p)
	if err != nil {
		return "", err
	}
	if relativePath == "." {
		return "", ErrInvalidPath
	}
	return relativePath, nil
}

func (r *Root) Stat(p string) (fs.FileInfo, error) {
	relativePath, err := rel(p)
	if err != nil {
		return nil, err
	}
	return r.root.Stat(relativePath)

}

func (r *Root) Open(p string) (*os.File, error) {
	relativePath, err := rel(p)
	if err != nil {
		return nil, err
	}
	return r.root.Open(relativePath)
}

func (r *Root) Create(p string) (*os.File, error) {
	relativePath, err := relMut(p)
	if err != nil {
		return nil, err
	}
	return r.root.Create(relativePath)
}

func (r *Root) MkdirAll(p string) error {
	relativePath, err := relMut(p)
	if err != nil {
		return err
	}
	return r.root.MkdirAll(relativePath, 0o755)
}

func (r *Root) Remove(p string) error {
	relativePath, err := relMut(p)
	if err != nil {
		return err
	}
	err = r.root.Remove(relativePath)
	if errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("remove %s: %w", p, ErrNotEmpty)
	}
	return err
}

func (r *Root) FS() fs.FS {
	return r.root.FS()
}

func (r *Root) Name() string {
	return r.root.Name()
}
