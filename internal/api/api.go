package api

import (
	"context"
	"encoding/json"
	"errors"
	"go-file-system/internal/disk"
	"go-file-system/internal/index"
	"go-file-system/internal/scan"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"time"
)

type Server struct {
	root    *disk.Root
	idx     *index.Index
	scanner *scan.Scanner
}

func New(root *disk.Root, idx *index.Index, scanner *scan.Scanner) *Server {
	return &Server{root: root, idx: idx, scanner: scanner}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/files", handler(s.listFiles))
	mux.Handle("GET /api/search", handler(s.search))
	mux.Handle("GET /api/stat", handler(s.stat))

	mux.Handle("GET /api/download", handler(s.download))

	mux.Handle("PUT /api/files", handler(s.upload))
	mux.Handle("DELETE /api/files", handler(s.deleteFile))
	mux.Handle("POST /api/dirs", handler(s.makeDir))

	mux.Handle("POST /api/rescan", handler(s.rescan))
	mux.Handle("GET /api/healthz", handler(s.healthz))
	return mux
}

type handler func(http.ResponseWriter, *http.Request) error

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := h(w, r)
	if err != nil {
		writeError(w, r, err)
	}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var errIndexStale = errors.New("written to disk but not indexed; call POST /api/rescan to repair")

var errIsDir = errors.New("path is a directory, not a file")

func status(err error) (int, string) {
	switch {
	case errors.Is(err, disk.ErrInvalidPath):
		return http.StatusBadRequest, "invalid_path"
	case errors.Is(err, errIsDir):
		return http.StatusBadRequest, "is_a_directory"
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, fs.ErrExist):
		return http.StatusConflict, "already_exists"
	case errors.Is(err, disk.ErrNotEmpty):
		return http.StatusConflict, "not_empty"
	case errors.Is(err, fs.ErrPermission):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, scan.ErrRunning):
		return http.StatusConflict, "conflict"
	case errors.Is(err, errIndexStale):
		return http.StatusInternalServerError, "index_stale"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	code, errCode := status(err)

	message := err.Error()
	if errCode == "internal" {
		message = "internal error"
		slog.Error("request failed",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"err", err)
	}

	writeJSON(w, code, errorBody{Error: errorDetail{Code: errCode, Message: message}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encoding response", "err", err)
	}
}

func parsePath(r *http.Request) string {
	return disk.Clean(r.URL.Query().Get("path"))
}

func parsePage(r *http.Request) (limit, offset int64) {
	q := r.URL.Query()

	limit, offset = 100, 0

	if n, err := strconv.ParseInt(q.Get("limit"), 10, 64); err == nil {
		limit = n
	}
	if n, err := strconv.ParseInt(q.Get("offset"), 10, 64); err == nil {
		offset = n
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

type entryJSON struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	IsDir bool   `json:"is_dir"`
	Ext   string `json:"ext"`
}

func newEntryJSON(e index.Entry) entryJSON {
	return entryJSON{
		Name:  e.Name,
		Path:  e.Path,
		Size:  e.Size,
		MTime: time.Unix(e.MTime, 0).UTC().Format(time.RFC3339),
		IsDir: e.IsDir,
		Ext:   e.Ext,
	}
}

type listJSON struct {
	Path    string      `json:"path,omitempty"`
	Total   int64       `json:"total"`
	Limit   int64       `json:"limit"`
	Offset  int64       `json:"offset"`
	Entries []entryJSON `json:"entries"`
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) error {
	p := parsePath(r)
	limit, offset := parsePage(r)
	q := r.URL.Query()

	entries, total, err := s.idx.List(r.Context(), p, q.Get("sort"), q.Get("order"), limit, offset)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, listJSON{
		Path:    p,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		Entries: toEntriesJSON(entries),
	})
	return nil
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) error {
	limit, offset := parsePage(r)
	q := r.URL.Query()

	entries, total, err := s.idx.Search(r.Context(), q.Get("q"), q.Get("ext"), limit, offset)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, listJSON{
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		Entries: toEntriesJSON(entries),
	})
	return nil
}

func (s *Server) stat(w http.ResponseWriter, r *http.Request) error {
	entry, err := s.idx.Stat(r.Context(), parsePath(r))
	if err != nil {

		return err
	}

	writeJSON(w, http.StatusOK, newEntryJSON(entry))
	return nil
}

func toEntriesJSON(entries []index.Entry) []entryJSON {
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, newEntryJSON(e))
	}
	return out
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) error {
	p := parsePath(r)
	f, err := s.root.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errIsDir
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
	return nil
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) error {
	p := parsePath(r)
	f, err := s.root.Create(p)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	e, err := s.index(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, newEntryJSON(e))
	return nil
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) error {
	p := parsePath(r)
	err := s.root.Remove(p)
	if err != nil {
		return err
	}
	err = s.idx.Delete(r.Context(), p)
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) makeDir(w http.ResponseWriter, r *http.Request) error {
	p := parsePath(r)
	if err := s.root.MkdirAll(p); err != nil {
		return err
	}
	for d := path.Dir(p); d != "/"; d = path.Dir(d) {
		_, err := s.index(r.Context(), d)
		if err != nil {
			return err
		}
	}
	e, err := s.index(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, newEntryJSON(e))
	return nil
}

func (s *Server) index(ctx context.Context, p string) (index.Entry, error) {
	fi, err := s.root.Stat(p)
	if err != nil {
		return index.Entry{}, err
	}
	e := scan.EntryFor(p, fi)
	err = s.idx.Upsert(ctx, e, s.scanner.Gen())
	if err != nil {

		slog.Error("write-through index failed", "path", p, "err", err)
		return index.Entry{}, errIndexStale
	}
	return e, nil
}

type rescanJSON struct {
	Scanned    int64 `json:"scanned"`
	Removed    int64 `json:"removed"`
	DurationMS int64 `json:"duration_ms"`
}

func (s *Server) rescan(w http.ResponseWriter, r *http.Request) error {
	result, err := s.scanner.Scan(r.Context())
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, rescanJSON{
		Scanned:    result.Scanned,
		Removed:    result.Removed,
		DurationMS: result.Duration.Milliseconds(),
	})
	return nil
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) error {
	_, err := w.Write([]byte("OK"))
	return err
}
