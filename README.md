

## `README.md`

```markdown
# Time Lapse Studio 

A web-based application that transforms videos into motion-blur time lapses using cumulative frame blending. Upload any video, select a blend mode, and generate a stylized time-lapse output in your browser.

<p align="center">
  <img src="assets/screenshot.png" alt="Time Lapse Studio UI" width="600"/>
</p>

## ✨ Features

- **Drag & Drop Upload** — Simple interface supporting MP4, MOV, MKV, AVI, WebM (up to 2 GiB)
- **Multiple Blend Modes** — Choose from 30+ FFmpeg blend modes (screen, overlay, difference, lighten, etc.)
- **Accurate Framerate Detection** — Handles variable frame rate (VFR) sources correctly
- **Real-Time Progress Tracking** — Monitor processing stages and completion percentage
- **Inline Playback** — Preview and download your time-lapse immediately after generation
- **Automatic Cleanup** — Jobs are purged after 2 hours to save disk space

##  How It Works

The app performs three phases of video processing:

1. **Extraction** — Source video is split into individual PNG frames at the probed framerate (e.g., 30 fps → 30 frames per second)
2. **Blending** — Frames are grouped by source second and cumulatively blended together (frame 1 as background, frames 2–N overlaid with selected mode)
3. **Encoding** — Blended frames are reassembled into an H.264 MP4 at the original video framerate

**Result:** A 120-second @ 30fps video → 120 blended frames → 4-second @ 30fps output (~30× speedup)

## ️ Prerequisites

- **Go 1.22+** — For building and running the server
- **FFmpeg** — Both `ffmpeg` and `ffprobe` binaries must be on your system PATH

### Installing FFmpeg

**macOS (Homebrew):**
```bash
brew install ffmpeg
```

**Linux (Ubuntu/Debian):**
```bash
sudo apt-get update && sudo apt-get install ffmpeg
```

**Windows (Chocolatey):**
```powershell
choco install ffmpeg
```

Verify installation:
```bash
ffmpeg -version
ffprobe -version
```

##  Installation

1. Clone the repository:
```bash
git clone https://github.com/yourusername/timelapse-studio.git
cd timelapse-studio
```

2. Initialize Go modules (if not already done):
```bash
go mod init timelapse
```

3. Download dependencies:
```bash
go mod tidy
```

##  Running Locally

Start the development server:

```bash
go run .
```

The server will listen on **http://localhost:9595/**

Open your browser and navigate to that URL to begin uploading videos.

##  Architecture

### Project Structure

```
timelapse-studio/
├── main.go                 # HTTP server + FFmpeg pipeline
├── go.mod                  # Go module definition
├── go.sum                  # Dependency checksums
├── README.md               # This file
└── static/
    └── index.html          # Frontend UI (embedded)
```

### Key Components

| Component | Description |
|-----------|-------------|
| **HTTP Server** | Standard library `net/http` with custom routes for upload/status/result |
| **Job Store** | In-memory job registry with mutex-protected concurrent access |
| **FFmpeg Pipeline** | 3-phase process: extract → blend → encode (runs as subprocess via `os/exec`) |
| **Blend Workers** | Up to 6 concurrent FFmpeg invocations for parallel group processing |
| **Janitor** | Background goroutine that cleans expired jobs every 10 minutes |

### API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Serve UI (static HTML) |
| `GET` | `/api/modes` | List supported blend modes (JSON) |
| `POST` | `/api/upload` | Upload video file (multipart/form-data) |
| `POST` | `/api/generate/{id}` | Start processing with selected blend mode |
| `GET` | `/api/job/{id}` | Poll job status and progress |
| `GET` | `/api/result/{id}?download=1` | Stream result video or download as attachment |

### Blend Modes Supported

Common options include:
- `screen`, `overlay`, `softlight`, `hardlight` — Creative lighting effects
- `addition`, `difference`, `exclude` — Motion trail emphasis
- `multiply`, `darken`, `lighten` — Exposure adjustments
- `grainmerge`, `grainextract` — Texture overlays

Full list depends on your installed FFmpeg version (shown in UI after launch).

##  Security Considerations

- **Upload limit:** Hard-coded at 2 GiB per file (adjust `maxUploadBytes` in code)
- **Job isolation:** Each job runs in its own temporary directory under `jobs/`
- **Path sanitization:** Job IDs are validated as 12-character hex strings (no path traversal)
- **No external network calls:** All processing is local; no data leaves your machine

##  Known Limitations

| Issue | Workaround |
|-------|------------|
| Very long videos (30+ minutes) may exhaust disk during frame extraction | Add windowed extraction (`-ss/-t` per group) — planned for v2 |
| Sub-1-second outputs for clips shorter than one blend group | Enforce minimum frame repetition — planned for v2 |
| No progress bar estimation (time varies by video complexity) | Linear progress shown; actual timing unpredictable |
| No audio passthrough (time-lapses are typically silent) | Audio is stripped intentionally (`-an` flag) |

## 欄 Contributing

Pull requests are welcome! For major changes, please open an issue first to discuss what you'd like to change.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

##  License

MIT License — see [`LICENSE`](LICENSE) for details.

##  FAQ

**Q: Why is my output much shorter than my source video?**  
A: This is expected. The app compresses time by blending N frames into one image. A 30fps source will produce an output ~30× faster than the original duration.

**Q: Can I change the output framerate?**  
A: Currently it matches the source framerate exactly. To change it, modify the `-framerate` flag in the encoding phase of `main.go`.

**Q: My ffmpeg blend-mode detection failed — what now?**  
A: The app falls back to a curated list of common modes. You can still use them, but detection helps prevent unsupported mode selection.

**Q: Where do the processed files go?**  
A: Jobs are stored in the `jobs/` directory relative to where you run `go run .`. They're automatically cleaned up after 2 hours.

---

Built with ❤️ using Go and FFmpeg. Happy blending! 
```

MIT License

Copyright (c) 2026 https://petroffspace.com

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
