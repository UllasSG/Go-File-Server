# go-file-system — Design

**Date:** 2026-08-29
**Status:** Approved, ready for implementation planning
**Module:** `go-file-system` · **Binary:** `gofs` · **Go:** 1.25.6

## Summary

A JSON HTTP API over a single directory tree, backed by a SQLite index that
serves fast directory listing and name search. Files live on disk as ordinary
files. The index is a cache and never the source of truth.

The project exists to learn Go: the standard library's HTTP server, `os.Root`,
`database/sql`, `log/slog`, `context`, and the testing package. It is
deliberately small and deliberately honest about its scope.

### What this is not

Not a NAS in the everyday sense, and not a VFS. There is no mountable network
protocol, so Finder, Windows Explorer, and editors cannot open files from it
directly — those speak SMB, NFS, or WebDAV, none of which this implements.
Interaction is via `curl` or a client program. "Editing" means download, edit
locally, re-upload.

Reachable from another machine on the same LAN over HTTP, so browse, search,
download, upload, and delete all work remotely. Mounting does not.

## Goals

1. Serve directory listings with sorting and pagination from the index.
2. Serve name search with an extension filter across the whole tree.
3. Upload, download, delete files; create directories.
4. Keep the index correct via a startup scan plus write-through on mutations,
   with a manual rescan endpoint.
5. Make every inconsistency recoverable by deleting the index and rescanning.

## Non-goals for v1

Web UI · authentication · move/rename · content hashing and dedup · recursive
directory size rollups · multiple named shares · recursive delete · symlink
following · WebDAV or any mount protocol · TLS · upload size limits.

Each is a candidate follow-on, listed in **Future phases** below.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Interface | JSON API, no UI | Focuses the work on API design and testing; a UI can be added without touching the core |
| Storage | Files on disk, SQLite index | Index enables sorted/paginated listing and tree-wide search |
| Index authority | Cache only, never truth | Any drift is repaired by `rm index.db` + rescan |
| Sync strategy | Startup full scan + write-through + manual rescan | Deterministic, easy to test; drift only from outside writers |
| Scanner reads | Metadata only (`Stat`), never file contents | Keeps a full scan fast; hashing deferred |
| Scope of root | One directory via `--root` | Multiple shares add a config layer without teaching anything new |
| Auth | None | LAN-only; token middleware is a clean phase-2 addition |
| Path in requests | Query parameter | Avoids URL-escaping and trailing-slash ambiguity |
| Upload encoding | Raw `PUT` body | `io.Copy` streams in constant memory; no HTML form exists to justify multipart |
| Router | `http.ServeMux` (Go 1.22+ patterns) | Method-aware routing without a dependency |
| SQLite driver | `modernc.org/sqlite` | Pure Go, no cgo, cross-compiles |
| Layout | Thin layers, one-way dependencies | Puts the disk-then-index ordering rule at one visible seam |

Sole external dependency: `modernc.org/sqlite`. Everything else is standard
library.

## Architecture

```
cmd/go-file-system/main.go   flags, wiring, initial scan, graceful shutdown
internal/disk/               root-jailed path resolution and file operations
internal/index/              SQLite schema, List/Search/Stat/Upsert/Delete
internal/scan/               full walk, reconcile disk against index
internal/api/                routing, handlers, JSON encoding, error mapping
```

Dependencies point one way: `api → {disk, index, scan}` and `scan → {disk,
index}`. `disk` and `index` know nothing about each other or about HTTP.

- **Reads** (`/api/files`, `/api/search`, `/api/stat`) hit `index` only.
- **Writes** hit `disk` first, then `index`.

That ordering is the central invariant. A failed disk write leaves the index
untouched; a failed index write leaves the disk correct and the index stale,
which is the direction to be wrong in.

Build:

```
go build -o gofs ./cmd/go-file-system
./gofs --root /Users/ullas/Files --db ./index.db --addr 0.0.0.0:8080
```

Flags: `--root` (required), `--db` (default `./index.db`), `--addr` (default
`127.0.0.1:8080`).

The address defaults to loopback deliberately: there is no auth in v1, so
binding to every interface must be a conscious act. Reaching it from another
machine on the LAN means `--addr 0.0.0.0:8080` plus three practical facts —
macOS prompts once to allow incoming connections; a sleeping laptop serves
nothing (`caffeinate -s ./gofs …` while testing); and DHCP will change the IP,
so prefer the Bonjour name macOS already publishes, `http://<hostname>.local:8080`.
Anyone on that network can then read and delete every file under `--root`.

## 1. API surface

### Reads — served from the index

| Endpoint | Purpose |
|---|---|
| `GET /api/files?path=&sort=&order=&limit=&offset=` | List one directory |
| `GET /api/search?q=&ext=&limit=&offset=` | Name search across the tree |
| `GET /api/stat?path=` | Metadata for a single entry |

`sort` ∈ {`name`, `size`, `mtime`}, default `name`. `order` ∈ {`asc`, `desc`},
default `asc`. `limit` default 100, maximum 1000. `offset` default 0.

### Content and mutations — disk, then index

| Endpoint | Purpose |
|---|---|
| `GET /api/download?path=` | File bytes via `http.ServeContent` (range requests, `If-Modified-Since`, content-type sniffing) |
| `PUT /api/files?path=` | Upload, streaming the raw request body. Overwrites |
| `DELETE /api/files?path=` | Delete a file, or an empty directory |
| `POST /api/dirs?path=` | Create a directory, parents included |

### Admin

| Endpoint | Purpose |
|---|---|
| `POST /api/rescan` | Force a full reconcile |
| `GET /api/healthz` | Liveness |

### Response shapes

Listing and search:

```json
{
  "path": "/photos",
  "total": 1284,
  "limit": 100,
  "offset": 0,
  "entries": [
    { "name": "img.jpg", "path": "/photos/img.jpg", "size": 204813,
      "mtime": "2026-08-21T10:03:11Z", "is_dir": false, "ext": "jpg" }
  ]
}
```

Search omits `path`. `stat` returns a bare entry object.

Rescan:

```json
{ "scanned": 51204, "removed": 17, "duration_ms": 1840 }
```

Insert and update are not distinguished — an upsert cannot report which it did
without contorting the SQL, and the distinction is not worth that.

Errors, one envelope everywhere:

```json
{ "error": { "code": "not_found", "message": "no such path: /photos/nope" } }
```

Codes: `invalid_path` (400), `forbidden` (403), `not_found` (404),
`already_exists` (409), `not_empty` (409), `conflict` (409, concurrent rescan),
`internal` (500).

## 2. Schema and queries

```sql
CREATE TABLE entries (
  path   TEXT PRIMARY KEY,  -- '/photos/img.jpg' — leading slash, no trailing
  parent TEXT NOT NULL,     -- '/photos'  (root itself has parent '')
  name   TEXT NOT NULL,     -- 'img.jpg'
  ext    TEXT NOT NULL,     -- 'jpg', lowercased; '' for dirs and extensionless
  size   INTEGER NOT NULL,  -- bytes; 0 for dirs
  mtime  INTEGER NOT NULL,  -- unix seconds
  is_dir INTEGER NOT NULL,  -- 0 or 1
  gen    INTEGER NOT NULL   -- scan generation, for mark-and-sweep
) STRICT;

CREATE INDEX idx_parent ON entries(parent, name);
CREATE INDEX idx_name   ON entries(name);
CREATE INDEX idx_gen    ON entries(gen);
```

`parent` is denormalised deliberately: it turns "list one directory" into
`WHERE parent = ?`, a single index seek, instead of a `LIKE '/photos/%'` scan
that also has to filter out grandchildren in Go.

`ext` is stored rather than computed so `?ext=pdf` is an indexed equality.
`mtime` is a unix integer for cheap sorting and no timezone ambiguity; the API
converts to RFC 3339 at the edge. `STRICT` makes SQLite enforce column types.

Pragmas on open:

```
journal_mode = WAL      readers don't block on the writer during a rescan
busy_timeout = 5000     wait rather than failing with SQLITE_BUSY
synchronous  = NORMAL   safe under WAL, meaningfully faster
```

Schema version tracked in `PRAGMA user_version`. No migration library.

### Listing

```sql
SELECT name, path, size, mtime, is_dir, ext FROM entries
WHERE parent = ?
ORDER BY is_dir DESC, <col> <dir>
LIMIT ? OFFSET ?;
```

Directories first, then the requested sort. `total` comes from a second
`SELECT COUNT(*) WHERE parent = ?`.

`ORDER BY` columns cannot be parameterised — SQLite binds `?` as a constant and
the sort silently does nothing — so `<col>` and `<dir>` are interpolated. This
is the one place in the project where building SQL by string concatenation is
correct, and it is safe only because both values come from a whitelist:

```go
var sortCols = map[string]string{"name": "name", "size": "size", "mtime": "mtime"}
col, ok := sortCols[q.Get("sort")]
if !ok { col = "name" }
```

Unknown input never reaches the query. Same treatment for `asc`/`desc`.

### Search

```sql
SELECT name, path, size, mtime, is_dir, ext FROM entries
WHERE name LIKE ? ESCAPE '\'
  AND (? = '' OR ext = ?)
  AND is_dir = 0
ORDER BY name LIMIT ? OFFSET ?;
```

`LIKE '%q%'` cannot use an index — the leading wildcard defeats it — so this is
a full table scan. Acceptable at this scale and stated explicitly so nobody
assumes `idx_name` is helping. FTS5 is the upgrade path if it ever matters.
`%` and `_` in the user's query are escaped before wrapping in wildcards.

### Upsert

```sql
INSERT INTO entries(path,parent,name,ext,size,mtime,is_dir,gen)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET
  size=excluded.size, mtime=excluded.mtime, gen=excluded.gen;
```

### Database location

The DB must not live under `--root`, or the scanner indexes its own database.
`--db` defaults to `./index.db` in the working directory, and the scanner skips
the DB path defensively regardless.

`*sql.DB` is a pool but SQLite permits one writer at a time; `busy_timeout`
covers this workload. A dedicated writer connection is the heavier fix and is
not needed here.

## 3. Scan and reconcile

### Mark and sweep

```
g := SELECT COALESCE(MAX(gen), 0) + 1 FROM entries
walk the tree, upserting every entry with gen = g
DELETE FROM entries WHERE gen < g
```

Adds, modifications, and deletions in one pass. The alternative — load all
known paths into a Go map and diff against the walk — needs memory proportional
to file count and more code; here the database performs the set difference and
Go's memory stays flat. Deriving `g` from `MAX(gen)` means the generation
survives restarts without a counter table.

### Batching

The entire walk runs in one transaction with one prepared statement. Outside a
transaction SQLite commits per statement, and each commit is an fsync — on a
50,000-file tree that is 50,000 fsyncs, the difference between roughly a second
and several minutes.

### Walk rules

- **Symlinks are skipped**, identified by `d.Type()&fs.ModeSymlink != 0` on the
  walk's `DirEntry`. The walk does not descend into them, so an unskipped
  symlink would be indexed as a zero-byte regular file that fails to download.
  Following them means handling cycles and out-of-root targets.
- **The DB file is skipped.**
- **Errors do not abort the scan.** A permission error or a file deleted
  mid-walk arrives as a non-nil `err` in the callback; the callback logs and
  returns `nil` to continue. Returning the error would abandon the whole tree
  over one unreadable file.
- The walk is confined by `fs.WalkDir(root.FS(), ".", …)`, so the scanner and
  the handlers share one enforcement mechanism rather than two that can drift.

### When scans run

**At startup, synchronously, before the listener opens.** No partially-built
index is ever visible. The cost is startup latency proportional to tree size;
serving immediately behind a `scanning` flag is the upgrade if that becomes
annoying.

**On `POST /api/rescan`**, the same function, guarded so two cannot overlap:

```go
if !s.mu.TryLock() { return ErrRunning }   // mapped to 409 conflict in §5
defer s.mu.Unlock()
```

`TryLock` rather than `Lock` — a second rescan gets an immediate 409 instead of
queueing.

### Write-through

| Operation | Order |
|---|---|
| `PUT /api/files` | stream body to disk → upsert row at current gen |
| `DELETE /api/files` | remove from disk → delete row |
| `POST /api/dirs` | `MkdirAll` → upsert row |

Write-through rows carry the **current** generation — the one the last scan
used, held in memory and updated after each scan. Getting this wrong means the
next rescan's sweep deletes every file uploaded since boot.

### Failure modes

| What fails | Result |
|---|---|
| Disk write | Index untouched; clean failure, error returned |
| Index write after successful disk write | File exists, invisible to listing and search until a rescan |
| Index delete after successful disk delete | Ghost row: listed, but download 404s |

An index write that fails after a successful disk write returns **500**, not
200. The bytes are safe, but the client's next listing will not show the file,
and silently succeeding into an invisible result is worse than an actionable
error. The message names the remedy: call `/api/rescan`.

**The invariant:** `rm index.db`, restart, and the system is fully correct
again. Nothing lives in the index that does not live on disk. `POST
/api/rescan` performs the same repair without a restart.

## 4. Path safety

`filepath.Join` cleans its result but does not confine it:

```go
filepath.Join("/srv/files", "../../etc/passwd")  →  "/etc/passwd"
```

The `strings.HasPrefix(full, root)` patch people reach for next still admits
`/srv/files-backup` against a root of `/srv/files`, and cannot see a symlink
escape at all, because that escape happens in the kernel after validation.

### Layer one — canonical virtual path

API paths are always slash-separated, so use `path`, not `filepath`:

```go
func clean(p string) string { return path.Clean("/" + p) }
```

Prefixing with `/` before cleaning is what makes escape impossible here: `..`
above `/` collapses to `/`, so no leading `..` survives. The result is
canonical — leading slash, no trailing slash, no `.` or `..`, no doubled
separators — and that string is what the index stores, so `/photos/img.jpg`,
`photos/img.jpg`, and `/photos/./img.jpg` are one row.

### Layer two — confined physical access

```go
root, err := os.OpenRoot(*rootFlag)   // stdlib, Go 1.24+
f, err := root.Open(rel)              // cleaned path minus leading slash
```

`*os.Root` holds a descriptor to the directory and resolves everything relative
to it with `openat`-style syscalls. Any component leaving the root — `..`, an
absolute symlink, a symlink chain pointing outside — returns an error. It also
resists the race a string check cannot: a directory swapped for a symlink
between validation and open. It is safe for concurrent use, which matters while
a rescan walks and handlers serve.

`Open`, `Create`, `OpenFile`, `Mkdir`, `MkdirAll`, `Remove`, `Stat`, `Lstat`,
and `FS()` cover the entire v1 surface.

Layer one produces canonical strings for the index; layer two enforces the
boundary in the kernel rather than in string comparison. Neither replaces the
other.

### Validation

Rejected with `400 invalid_path` before touching disk: paths containing a NUL
byte; paths longer than 4096 bytes; `PUT`, `DELETE`, or `POST /api/dirs`
targeting `/` itself.

`DELETE` uses `Remove`, never `RemoveAll` — it deletes a file or an empty
directory. One query-parameter typo must not erase a tree.

Directories are created `0755`, files `0644`, both subject to umask.

### macOS case-insensitivity

APFS is case-insensitive by default: the disk treats `Foo.txt` and `foo.txt` as
one file, while `TEXT PRIMARY KEY` treats them as two rows, leaving a ghost.
Not special-cased in v1 — mark-and-sweep converges, since the next scan stamps
only the row matching what the disk reports and sweeps the other. A whole class
of bug reduces to "the next scan fixes it."

## 5. Errors and code shape

### Sentinels

`os.Root` returns `*fs.PathError` wrapping the standard sentinels, so only what
the stdlib lacks is defined:

```go
// internal/disk
var (
    ErrInvalidPath = errors.New("invalid path")
    ErrNotEmpty    = errors.New("directory not empty")
)

// internal/scan
var ErrRunning = errors.New("scan already running")
```

`fs.ErrNotExist`, `fs.ErrExist`, and `fs.ErrPermission` pass through, wrapped
with `%w` so `errors.Is` still matches.

### One place that knows HTTP

```go
func status(err error) (int, string) {
    switch {
    case errors.Is(err, disk.ErrInvalidPath): return 400, "invalid_path"
    case errors.Is(err, fs.ErrNotExist):      return 404, "not_found"
    case errors.Is(err, fs.ErrExist):         return 409, "already_exists"
    case errors.Is(err, disk.ErrNotEmpty):    return 409, "not_empty"
    case errors.Is(err, fs.ErrPermission):    return 403, "forbidden"
    case errors.Is(err, scan.ErrRunning):     return 409, "conflict"
    default:                                  return 500, "internal"
    }
}
```

**`internal/api` never imports `modernc.org/sqlite` and never inspects a driver
error code.** `internal/index` recognises constraint violations and returns
`fs.ErrExist`; the API layer only sees errors it already understands. Reversing
this would mean editing every handler to change storage engines.

500s do not leak: the client gets `{"error":{"code":"internal","message":"internal
error"}}` plus a request ID, while the full error — with paths and SQL — goes
to the log under that ID.

### Handlers returning errors

```go
type handler func(http.ResponseWriter, *http.Request) error

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if err := h(w, r); err != nil { writeError(w, r, err) }
}
```

Handlers `return err`; one function decides what that means on the wire. This
is the same idiom as `http.HandlerFunc` — a method on a function type
satisfying an interface.

### Middleware

Outermost first: **recover → log → handler**. No auth in v1.

Recovery turns a panic into a logged stack trace and a 500. Logging uses
`log/slog` and records method, path, status, duration, and bytes written.

Capturing the status requires wrapping `http.ResponseWriter` to intercept
`WriteHeader`, which hides the optional interfaces the real writer implements.
`http.ServeContent` checks for `io.ReaderFrom` to use `sendfile`, so the
wrapper must delegate `ReadFrom` or every download becomes a userspace copy.

### Server configuration

```go
srv := &http.Server{
    ReadHeaderTimeout: 10 * time.Second,
    IdleTimeout:       120 * time.Second,
    // WriteTimeout intentionally unset
}
```

`WriteTimeout` caps the total time to write a response, which truncates any
download slower than the timeout — a bug on a file server. `ReadHeaderTimeout`
provides the slowloris protection actually wanted; `IdleTimeout` reaps dead
keep-alives.

No upload size cap in v1.

Handlers pass `r.Context()` into `QueryContext`/`ExecContext`, so a
disconnecting client cancels its query.

### Startup and shutdown

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
```

`main` opens the DB, opens the root, runs the initial scan, starts the
listener, waits on that context, then calls `srv.Shutdown` with a timeout so
in-flight uploads finish, then `db.Close()` so WAL checkpoints cleanly. `main`
is wiring only.

## 6. Testing

No mocks. `t.TempDir()` gives a real directory, cleaned up automatically;
SQLite is a file.

**Do not use `:memory:` for the test DB.** `*sql.DB` is a pool, and with
`modernc.org/sqlite` each pooled connection opens its own separate in-memory
database — a row written on one connection is invisible on the next, producing
intermittent failures that make no sense. A file under `t.TempDir()` avoids it
and matches production.

Table-driven throughout, `httptest` for handlers, `go test -race` in the loop
for the rescan mutex and concurrent handlers.

### Written first — path safety

```go
for _, bad := range []string{
    "../etc/passwd", "/../../etc/passwd", "....//etc/passwd",
    "/photos/../../etc/passwd", "\x00", strings.Repeat("a", 5000),
} { /* every one rejected, never resolved */ }
```

Plus the case string-checking cannot catch: `os.Symlink("/etc",
filepath.Join(root, "escape"))`, then request `/escape/passwd` — it must fail.
That test proves `os.Root` is doing its job and that nobody replaced it with
`filepath.Join` plus a prefix check.

### Reconcile

One test per transition, against a scanned tree: add a file → rescan → row
appears; delete → rescan → row disappears; modify → rescan → size and mtime
update; rename a directory on disk → rescan → old subtree gone, new present.

Specifically for the generation bug: **upload via the API, rescan, assert the
file is still indexed.** Nothing else catches a wrong write-through generation.

### The invariant, as a test

Build a tree. Scan. Mutate through the API — uploads, deletes, mkdirs. Then
`DELETE FROM entries`, rescan from scratch, and assert the resulting index is
identical to what write-through produced. This is "the index is only a cache"
stated as an executable property.

### The rest

- `?sort=name;DROP TABLE entries` returns 400, and the table survives.
- Searching for `100%_real.txt` matches that file only — `%`/`_` escaping.
- Pagination past the end returns an empty list, not an error.
- `PUT` several megabytes, `GET` it back, bytes identical — large enough to
  exercise streaming rather than one buffer.
- A `Range` request returns 206 with the correct slice.
- Error mapping: missing path 404, non-empty directory delete 409, malformed
  path 400.

Not tested: SQLite, `net/http`, `os.Root`. No coverage target — the path-safety
table and the invariant test are worth more than a percentage.

Optional: a benchmark comparing per-statement commits against the batched
transaction, to see the difference firsthand.

## Future phases

Each is self-contained and builds on v1 without restructuring it.

1. **Token auth** — a middleware and a `--token` flag. Smallest useful next step.
2. **Content hashing** — a worker pool hashing file contents during the scan,
   skipping files whose mtime and size are unchanged. Adds duplicate detection
   and integrity checks, and is the natural place to learn goroutines,
   channels, and `errgroup`.
3. **Move/rename** — `os.Rename` on disk, plus rewriting the path of every
   descendant row in the index. Easier once reconcile is trusted.
4. **FTS5 search** — replaces the `LIKE` scan when the tree gets large.
5. **Directory size rollups** — recursive size and file count per directory.
6. **WebDAV** — implement `webdav.FileSystem` over `internal/disk` and mount it
   in Finder. This is what turns the project into an actual NAS, and is the
   single highest-value addition if that becomes the goal.
7. **A web UI** — the JSON API was designed so this changes nothing underneath.
