package scan

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-file-system/internal/disk"
	"go-file-system/internal/index"
)

var ErrRunning = errors.New("scan already running")

type Result struct {
	Scanned  int64
	Removed  int64
	Duration time.Duration
}

type Scanner struct {
	root   *disk.Root
	idx    *index.Index
	mu     sync.Mutex
	gen    atomic.Int64
	dbPath string

	skipRel string
}

func New(root *disk.Root, idx *index.Index, dbPath string) (*Scanner, error) {
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	s := &Scanner{root: root, idx: idx, dbPath: dbPath}

	if absRoot, err := filepath.Abs(root.Name()); err == nil {
		if rel, err := filepath.Rel(absRoot, dbPath); err == nil && !strings.HasPrefix(rel, "..") {
			s.skipRel = filepath.ToSlash(rel)
		}
	}
	return s, nil
}

func (s *Scanner) Gen() int64 {
	return s.gen.Load()
}

func (s *Scanner) Scan(ctx context.Context) (Result, error) {

	if !s.mu.TryLock() {
		return Result{}, ErrRunning
	}
	defer s.mu.Unlock()

	start := time.Now()
	gen, err := s.idx.NextGen(ctx)
	if err != nil {
		return Result{}, err
	}

	s.gen.Store(gen)

	tx, err := s.idx.BeginScan(ctx)
	if err != nil {
		return Result{}, err
	}

	defer tx.Rollback()

	var scanned int64
	walkErr := fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {

			slog.Warn("scan: skipping unreadable entry", "path", p, "err", err)
			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if s.skipRel != "" && (p == s.skipRel || strings.HasPrefix(p, s.skipRel+"-")) {
			return nil
		}

		fi, err := d.Info()
		if err != nil {

			slog.Warn("scan: stat failed", "path", p, "err", err)
			return nil
		}

		if err := tx.Upsert(ctx, EntryFor(disk.Clean(p), fi), gen); err != nil {

			return err
		}
		scanned++
		return nil
	})
	if walkErr != nil {
		return Result{}, walkErr
	}

	if err := tx.Commit(); err != nil {
		return Result{}, err
	}

	removed, err := s.idx.Sweep(ctx, gen)
	if err != nil {
		return Result{}, err
	}

	return Result{Scanned: scanned, Removed: removed, Duration: time.Since(start)}, nil
}

func EntryFor(virtual string, fi fs.FileInfo) index.Entry {
	e := index.Entry{
		Path:   virtual,
		Parent: path.Dir(virtual),
		Name:   path.Base(virtual),
		MTime:  fi.ModTime().Unix(),
		IsDir:  fi.IsDir(),
	}

	if virtual == "/" {
		e.Parent = ""
		e.Name = ""
	}

	if !e.IsDir {
		e.Size = fi.Size()
		e.Ext = strings.TrimPrefix(strings.ToLower(path.Ext(virtual)), ".")
	}
	return e
}
