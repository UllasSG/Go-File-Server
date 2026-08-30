# Go File Server

Lightweight HTTP file server in Go. Serves one directory tree as a JSON API, backed by a SQLite index for fast listing and search.

## Run

```
go build -o gofs ./cmd/go-file-system
./gofs -root /path/to/serve -db ./index.db -addr 127.0.0.1:8080
```

Keep `-db` outside `-root`. Scans the tree on startup, then listens.

## API

```
GET    /api/files?path=&sort=&order=&limit=&offset=
GET    /api/search?q=&ext=
GET    /api/stat?path=
GET    /api/download?path=
PUT    /api/files?path=      (raw body)
DELETE /api/files?path=
POST   /api/dirs?path=
POST   /api/rescan
GET    /api/healthz
```

## Example

```
curl "localhost:8080/api/files"
curl -X PUT --data-binary @photo.jpg "localhost:8080/api/files?path=/photo.jpg"
curl "localhost:8080/api/download?path=/photo.jpg" -o out.jpg
```

Paths are confined with `os.Root`. No auth. Delete the index and restart to rebuild it.
