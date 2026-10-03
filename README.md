# Time Lapse Studio

A small Go web app that turns a video into a time lapse by blending frames
together: every second of footage (one group of frames) becomes a single
blended frame, using any of ffmpeg's blend modes.

![screenshot](assets/screenshot.png)

## Requirements

- Go 1.22+
- `ffmpeg` and `ffprobe` on `PATH` (the blend modes offered are detected from
  the local ffmpeg build at startup)

## Running

```sh
go run .
# or
go build -o timelapse . && ./timelapse
```

Then open <http://localhost:9595/>, drop a video, pick a blend mode and click
**Generate**. If processing fails, the error is shown with a **Start over**
button. Ctrl+C shuts the server down gracefully and aborts running jobs.

Job files live under `./jobs/` (relative to the working directory). Jobs are
kept in memory only, so any leftover job directories are removed on startup.

## Building for other platforms

The release archive contains source only. Go cross-compiles without extra
tooling; the web UI is embedded, so the result is a single self-contained
binary (it still needs `ffmpeg`/`ffprobe` on `PATH` on the target machine).
Set `GOOS`/`GOARCH` for the target:

```sh
# Linux
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o timelapse-linux-amd64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o timelapse-linux-arm64 .
# macOS (Intel / Apple Silicon)
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o timelapse-darwin-amd64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o timelapse-darwin-arm64 .
# Windows
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o timelapse-windows-amd64.exe .
```

On Windows PowerShell, set the variables first instead:

```powershell
$env:CGO_ENABLED="0"; $env:GOOS="windows"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w" -o timelapse.exe .
```

`go tool dist list` shows every supported `GOOS/GOARCH` pair.

## How it works

1. **Upload**: the video is streamed to `jobs/<id>/` and probed with `ffprobe`
   for its frame rate and duration. The group size (`chunk`) is the frame rate
   rounded, clamped to 1–120.
2. **Generate**: a single ffmpeg pass:
   - normalises to the source frame rate and converts to planar RGB;
   - blends each group of `chunk` consecutive frames into one frame:
     - **most modes**: the stream is split into `chunk` branches that are
       chained through `blend=all_mode=<mode>`, so frame 1 is blended with
       frame 2, that result with frame 3, and so on;
     - **`average`**: `tmix` gives every frame in the group equal weight (a
       chained pairwise average would favour the latest frames);
   - encodes the result as H.264 MP4 at the original frame rate, so the output
     plays about `chunk`× faster than the source.
3. **Result**: served for preview and download. The uploaded input is deleted
   once the result is ready; failed jobs are deleted immediately.

### Notes

- **`average` drops a partial tail**: the last frames that don't fill a whole
  group (under one second of footage) produce no output frame in `average`
  mode. Other modes blend them into a final, partial frame. A clip shorter than
  one group fails with an error in `average` mode.
- **Memory**: all frames of a group are held in memory while it is blended.
  Expect roughly 1.8 GB peak per job for 1080p at 120 fps (the largest group
  size); 4K at high frame rates needs several GB. Lower `maxConcurrentJobs` or
  `maxChunk` on small machines.
- **Exposure**: the server listens on all interfaces and has no
  authentication or limit on concurrent uploads (each may use up to 2 GiB of
  disk for up to `jobTTL`). For anything beyond local use, set `listenAddr` to
  `127.0.0.1:9595` or put it behind an authenticating reverse proxy.
- Frame and group counts are estimated from the duration while processing and
  shown with `≈` until the job finishes.

## Configuration

Settings are constants at the top of `main.go`:

| Constant            | Default  | Meaning                                           |
|---------------------|----------|---------------------------------------------------|
| `listenAddr`        | `:9595`  | HTTP listen address                               |
| `maxUploadBytes`    | 2 GiB    | Upload size limit (larger uploads get HTTP 413)   |
| `maxConcurrentJobs` | 2        | Jobs processed at once; others wait as `queued`   |
| `maxChunk`          | 120      | Maximum frames per blend group                    |
| `maxSaneFPS`        | 240      | Higher reported frame rates are treated as bogus  |
| `maxProcessTime`    | 2 h      | Per-job processing timeout                        |
| `jobTTL`            | 2 h      | Finished, failed and never-started jobs are deleted after this |

## HTTP API

| Method & path              | Description                                              |
|----------------------------|----------------------------------------------------------|
| `GET /api/modes`           | `{"modes": [...]}`: blend modes supported by local ffmpeg |
| `POST /api/upload`         | multipart form, field `video`; returns the job            |
| `POST /api/generate/{id}`  | JSON `{"mode": "screen"}`; starts processing (202)        |
| `GET /api/job/{id}`        | job status and progress                                  |
| `GET /api/result/{id}`     | the MP4 (`?download=1` sends it as an attachment)        |

Job status goes `uploaded` → `queued` → `processing` → `done` / `failed`.
`generate` returns 409 if the job has already started, finished or failed.

```sh
id=$(curl -s -F video=@clip.mp4 localhost:9595/api/upload | jq -r .id)
curl -s -X POST -d '{"mode":"lighten"}' localhost:9595/api/generate/$id
curl -s localhost:9595/api/job/$id
curl -s -o out.mp4 "localhost:9595/api/result/$id?download=1"
```

## License

Released under the [MIT License](LICENSE).

---

(c) petroffspace.com
