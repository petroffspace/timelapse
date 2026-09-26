package main

import (
	"context"
	"crypto/rand"
	_ "embed" // required for //go:embed directive
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/index.html
var indexHTML []byte

const (
	listenAddr        = ":9595"
	maxUploadBytes    = 2 << 30 // 2 GiB
	maxProcessTime    = 2 * time.Hour
	jobTTL            = 2 * time.Hour
	framesDirName     = "frames"
	blendedDirName    = "blended"
	resultName        = "result.mp4"
	maxBlendWorkers   = 6
	maxConcurrentJobs = 2
	jobsRoot          = "jobs"
	fallbackModesStr  = "addition addition128 and average bleach burn darken difference divide dodge extremity exclusion freeze glow grainextract grainmerge hardlight hardmix heat lighten linearlight multiply multiply128 negation normal or overlay phoenix pinlight reflect screen softlight subtract vividlight xor"
)

var (
	idRe        = regexp.MustCompile(`^[0-9a-f]{12}$`)
	alnumRe     = regexp.MustCompile(`^[a-z0-9]+$`)
	modeTokenRe = regexp.MustCompile(`^[a-z0-9]+$`)
	allowedModes                 = map[string]bool{}
)

// ============================================================ job model

type Status string

const (
	StatusUploaded   Status = "uploaded"
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
)

type Job struct {
	ID string

	mux         sync.Mutex
	Status      Status
	Stage       string
	Progress    float64
	BlendMode   string
	FPSFraction string
	FPS         float64
	Chunk       int
	InputName   string
	TotalFrames int
	TotalGroups int
	GroupsDone  int
	Error       string
	CreatedAt   time.Time
	FinishedAt  time.Time
}

func (j *Job) mutate(fn func(*Job)) { j.mux.Lock(); fn(j); j.mux.Unlock() }

func (j *Job) setStage(stage string, progress float64) {
	j.mutate(func(jj *Job) { jj.Stage = stage; jj.Progress = progress })
	log.Printf("job %s stage=%s (%.1f%%)", j.ID, stage, progress)
}

func (j *Job) fail(err error) {
	log.Printf("job %s FAILED: %v", j.ID, err)
	j.mutate(func(jj *Job) {
		jj.Status = StatusFailed
		jj.Error = err.Error()
		jj.Stage = "failed"
		jj.FinishedAt = time.Now()
	})
}

func (j *Job) finishOK() {
	j.mutate(func(jj *Job) {
		jj.Status = StatusDone
		jj.Stage = "done"
		jj.Progress = 100
		jj.FinishedAt = time.Now()
	})
	log.Printf("job %s DONE: %d frames -> %d blended groups", j.ID, j.TotalFrames, j.TotalGroups)
}

type jobDTO struct {
	ID          string  `json:"id"`
	Status      Status  `json:"status"`
	Stage       string  `json:"stage,omitempty"`
	Progress    float64 `json:"progress"`
	BlendMode   string  `json:"blend_mode,omitempty"`
	FPS         float64 `json:"fps,omitempty"`
	Chunk       int     `json:"chunk,omitempty"`
	TotalFrames int     `json:"total_frames,omitempty"`
	TotalGroups int     `json:"total_groups,omitempty"`
	GroupsDone  int     `json:"groups_done,omitempty"`
	Error       string  `json:"error,omitempty"`
	ResultURL   string  `json:"result_url,omitempty"`
	DownloadURL string  `json:"download_url,omitempty"`
}

func (j *Job) dto() jobDTO {
	j.mux.Lock()
	defer j.mux.Unlock()
	d := jobDTO{
		ID: j.ID, Status: j.Status, Stage: j.Stage, Progress: j.Progress,
		BlendMode: j.BlendMode, FPS: j.FPS, Chunk: j.Chunk,
		TotalFrames: j.TotalFrames, TotalGroups: j.TotalGroups, GroupsDone: j.GroupsDone, Error: j.Error,
	}
	if j.Status == StatusDone {
		d.ResultURL = "/api/result/" + j.ID
		d.DownloadURL = d.ResultURL + "?download=1"
	}
	return d
}

func (j *Job) statusSnapshot() (Status, time.Time) {
	j.mux.Lock(); defer j.mux.Unlock()
	return j.Status, j.FinishedAt
}

// ============================================================ store

type store struct {
	mu sync.RWMutex
	m  map[string]*Job
}

func newStore() *store { return &store{m: map[string]*Job{}} }

func (s *store) add(j *Job)       { s.mu.Lock(); s.m[j.ID] = j; s.mu.Unlock() }
func (s *store) get(id string) (*Job, bool) {
	s.mu.RLock(); defer s.mu.RUnlock()
	j, ok := s.m[id]
	return j, ok
}
func (s *store) purge(id string) { s.mu.Lock(); delete(s.m, id); s.mu.Unlock() }

// ============================================================ app

type app struct {
	store *store
	sem   chan struct{}
}

func newApp() *app {
	return &app{
		store: newStore(),
		sem:   make(chan struct{}, maxConcurrentJobs),
	}
}

func (a *app) jobDir(id string) string { return filepath.Join(jobsRoot, id) }

// ============================================================ ffmpeg

func runFFmpeg(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		snippet := string(out)
		if len(snippet) > 500 {
			snippet = snippet[len(snippet)-500:]
		}
		return fmt.Errorf("ffmpeg failed: %w: %s", err, snippet)
	}
	return nil
}

func detectBlendModes() []string {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-h", "filter=blend").CombinedOutput()
	if err == nil {
		var modes []string
		inEnums := false
		for _, ln := range strings.Split(string(out), "\n") {
			trimmed := strings.TrimSpace(ln)
			if !inEnums {
				if strings.HasPrefix(trimmed, "all_mode") {
					inEnums = true
				}
				continue
			}
			if trimmed == "" {
				continue
			}
			tok := strings.Fields(trimmed)[0]
			if len(ln) > 0 && ln[0] == ' ' && modeTokenRe.MatchString(tok) {
				modes = append(modes, tok)
			} else {
				break
			}
		}
		if len(modes) > 0 {
			sort.Strings(modes)
			log.Printf("detected %d blend modes from local ffmpeg", len(modes))
			return modes
		}
	}
	log.Println("blend-mode autodetection failed; using fallback list")
	modes := strings.Fields(fallbackModesStr)
	sort.Strings(modes)
	return modes
}

func parseFraction(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	num, den := s, "1"
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num, den = s[:i], s[i+1:]
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 || n <= 0 {
		return 0, false
	}
	return n / d, true
}

type probeInfo struct {
	FPSFraction string
	FPS         float64
}

func probeVideo(path string) (probeInfo, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate,avg_frame_rate",
		"-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return probeInfo{}, fmt.Errorf("ffprobe failed: %w", err)
	}
	var parsed struct {
		Streams []struct {
			RFrameRate   string `json:"r_frame_rate"`
			AvgFrameRate string `json:"avg_frame_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Streams) == 0 {
		return probeInfo{}, errors.New("no video stream found or unreadable ffprobe output")
	}
	st := parsed.Streams[0]
	frac := st.RFrameRate
	if _, ok := parseFraction(frac); !ok {
		frac = st.AvgFrameRate
	}
	if _, ok := parseFraction(frac); !ok {
		frac = "25/1"
	}
	fps, _ := parseFraction(frac)
	return probeInfo{FPSFraction: frac, FPS: fps}, nil
}

func frameName(n int) string { return fmt.Sprintf("frame_%06d.png", n) }

func listPNGs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func min(a, b int) int { if a < b { return a }; return b }

// ============================================================ pipeline

func process(ctx context.Context, j *Job, blendMode string, a *app) {
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		j.fail(errors.New("cancelled")); return
	}

	ctx, cancel := context.WithTimeout(ctx, maxProcessTime)
	defer cancel()

	j.mutate(func(jj *Job) { jj.Status = StatusProcessing; jj.BlendMode = blendMode })

	dir := a.jobDir(j.ID)
	inputPath := filepath.Join(dir, j.InputName)
	framesDir := filepath.Join(dir, framesDirName)
	blendedDir := filepath.Join(dir, blendedDirName)

	j.setStage("extracting_frames", 0)
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		j.fail(err); return
	}
	if err := runFFmpeg(ctx,
		"-i", inputPath,
		"-vf", "fps="+j.FPSFraction,
		"-vsync", "0",
		"-start_number", "1",
		filepath.Join(framesDir, "frame_%06d.png"),
	); err != nil {
		j.fail(fmt.Errorf("frame extraction failed: %w", err)); return
	}

	frames, err := listPNGs(framesDir)
	if err != nil {
		j.fail(err); return
	}
	total := len(frames)
	if total == 0 {
		j.fail(errors.New("no frames extracted")); return
	}
	chunk := j.Chunk
	groups := (total + chunk - 1) / chunk
	j.mutate(func(jj *Job) {
		jj.TotalFrames = total
		jj.TotalGroups = groups
	})

	j.setStage("blending_groups", 2)
	if err := os.MkdirAll(blendedDir, 0o755); err != nil {
		j.fail(err); return
	}

	errCh := make(chan error, groups)
	var wg sync.WaitGroup
	work := make(chan int, groups)
	for g := 0; g < groups; g++ {
		work <- g
	}
	close(work)

	workers := maxBlendWorkers
	if workers > runtime.NumCPU() { workers = runtime.NumCPU() }
	if workers > groups { workers = groups }
	if workers < 1 { workers = 1 }

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range work {
				if err := blendGroup(ctx, j, g, chunk, total, framesDir, blendedDir, blendMode); err != nil {
					errCh <- fmt.Errorf("group %d: %w", g, err)
					cancel()
					return
				}
				j.mutate(func(jj *Job) { jj.GroupsDone++ })
				done := 0
				j.mux.Lock(); done = j.GroupsDone; j.mux.Unlock()
				j.mutate(func(jj *Job) {
					jj.Progress = 2 + 88*float64(done)/float64(groups)
				})
			}
		}()
	}
	wg.Wait()
	close(errCh)

	if err := <-errCh; err != nil {
		j.fail(err); return
	}

	os.RemoveAll(framesDir)

	j.setStage("encoding_video", 92)
	if err := runFFmpeg(ctx,
		"-framerate", j.FPSFraction, // ORIGINAL fps, not 1
		"-start_number", "1",
		"-i", filepath.Join(blendedDir, "frame_%06d.png"),
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p",
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", "18",
		"-movflags", "+faststart",
		"-an",
		filepath.Join(dir, resultName),
	); err != nil {
		j.fail(fmt.Errorf("video encoding failed: %w", err)); return
	}

	os.RemoveAll(blendedDir)
	j.finishOK()
}

func blendGroup(ctx context.Context, j *Job, g, chunk, total int, framesDir, blendedDir, mode string) error {
	first := g*chunk + 1
	last := min((g+1)*chunk, total)
	m := last - first + 1

	src := make([]string, m)
	for i := 0; i < m; i++ {
		src[i] = filepath.Join(framesDir, frameName(first+i))
	}

	if _, err := os.Stat(src[0]); err != nil {
		return fmt.Errorf("background frame missing: %w", err)
	}

	outPath := filepath.Join(blendedDir, frameName(g+1))

	if m == 1 {
		data, err := os.ReadFile(src[0])
		if err != nil {
			return err
		}
		return os.WriteFile(outPath, data, 0o644)
	}

	args := make([]string, 0, 2*m+12)
	for _, p := range src {
		args = append(args, "-i", p)
	}

	var fc strings.Builder
	prev := "0:v"
	for k := 1; k < m; k++ {
		fmt.Fprintf(&fc, "[%s][%d:v]blend=all_mode=%s[b%d];", prev, k, mode, k)
		prev = fmt.Sprintf("b%d", k)
	}
	graph := strings.TrimSuffix(fc.String(), ";")
	if graph == "" {
		return errors.New("empty filter graph")
	}

	args = append(args,
		"-filter_complex", graph,
		"-map", "["+prev+"]",
		"-frames:v", "1",
		"-update", "1",
		outPath,
	)

	if err := runFFmpeg(ctx, args...); err != nil {
		return err
	}

	for _, p := range src {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ============================================================ handlers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *app) sanitizeExtension(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mov", ".mkv", ".avi", ".webm", ".m4v", ".mpg", ".mpeg", ".ts":
		return ext
	default:
		return ".bin"
	}
}

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonErr(w, http.StatusBadRequest, "upload too large or malformed")
		return
	}
	file, hdr, err := r.FormFile("video")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, `missing field "video"`)
		return
	}
	defer file.Close()

	id := newID()
	dir := a.jobDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "cannot create job directory"); return
	}

	inName := "input" + a.sanitizeExtension(hdr.Filename)
	dst, err := os.Create(filepath.Join(dir, inName))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "cannot write file"); return
	}
	size, err := io.Copy(dst, file)
	dst.Close()
	if err != nil {
		os.RemoveAll(dir)
		jsonErr(w, http.StatusInternalServerError, "failed saving upload"); return
	}
	if size == 0 {
		os.RemoveAll(dir)
		jsonErr(w, http.StatusBadRequest, "empty file"); return
	}

	info, err := probeVideo(filepath.Join(dir, inName))
	if err != nil {
		os.RemoveAll(dir)
		jsonErr(w, http.StatusBadRequest, "not a readable video: "+err.Error()); return
	}

	chunk := int(math.Round(info.FPS))
	if chunk < 1 { chunk = 1 }
	if chunk > 120 { chunk = 120 }

	j := &Job{
		ID: id, Status: StatusUploaded, Stage: "awaiting_generate", CreatedAt: time.Now(),
		FPSFraction: info.FPSFraction, FPS: math.Round(info.FPS*1000) / 1000, Chunk: chunk, InputName: inName,
	}
	a.store.add(j)
	log.Printf("upload %s: %.1f MiB, fps=%s (%.3f) -> chunk=%d", id, float64(size)/(1<<20), info.FPSFraction, info.FPS, chunk)

	writeJSON(w, http.StatusOK, j.dto())
}

func (a *app) handleGenerate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		jsonErr(w, http.StatusNotFound, "unknown job"); return
	}
	j, ok := a.store.get(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "unknown job"); return
	}

	var body struct{ Mode string `json:"mode"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Mode == "" {
		jsonErr(w, http.StatusBadRequest, `missing "mode" in body`); return
	}
	mode := strings.ToLower(body.Mode)
	if !alnumRe.MatchString(mode) || !allowedModes[mode] {
		jsonErr(w, http.StatusBadRequest, "unsupported blend mode: "+body.Mode); return
	}

	st, _ := j.statusSnapshot()
	switch st {
	case StatusProcessing:
		jsonErr(w, http.StatusConflict, "already processing"); return
	case StatusDone:
		jsonErr(w, http.StatusConflict, "already finished"); return
	case StatusFailed:
		jsonErr(w, http.StatusConflict, "job previously failed"); return
	}

	go process(context.Background(), j, mode, a)
	writeJSON(w, http.StatusAccepted, j.dto())
}

func (a *app) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		jsonErr(w, http.StatusNotFound, "unknown job"); return
	}
	j, ok := a.store.get(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "unknown job"); return
	}
	writeJSON(w, http.StatusOK, j.dto())
}

func (a *app) handleResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		jsonErr(w, http.StatusNotFound, "unknown job"); return
	}
	j, ok := a.store.get(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "unknown job"); return
	}
	if st, _ := j.statusSnapshot(); st != StatusDone {
		jsonErr(w, http.StatusConflict, "result not ready"); return
	}
	path := filepath.Join(a.jobDir(id), resultName)
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="timelapse_`+id+`.mp4"`)
	}
	http.ServeFile(w, r, path)
}

func handleModes(w http.ResponseWriter, r *http.Request) {
	modes := make([]string, 0, len(allowedModes))
	for m := range allowedModes {
		modes = append(modes, m)
	}
	sort.Strings(modes)
	writeJSON(w, http.StatusOK, map[string]any{"modes": modes})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// ============================================================ janitor

func (a *app) janitor() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		cutoff := time.Now().Add(-jobTTL)
		for _, id := range a.listIDs() {
			j, ok := a.store.get(id)
			if !ok { continue }
			st, fin := j.statusSnapshot()
			if st == StatusDone || st == StatusFailed {
				if fin.Before(cutoff) {
					os.RemoveAll(a.jobDir(id))
					a.store.purge(id)
					log.Printf("janitor: expired job %s", id)
				}
			} else if st == StatusUploaded && j.CreatedAt.Before(cutoff) {
				os.RemoveAll(a.jobDir(id))
				a.store.purge(id)
				log.Printf("janitor: expired unstarted job %s", id)
			}
		}
	}
}

func (a *app) listIDs() []string {
	a.store.mu.RLock(); defer a.store.mu.RUnlock()
	ids := make([]string, 0, len(a.store.m))
	for id := range a.store.m {
		ids = append(ids, id)
	}
	return ids
}

// ============================================================ main

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	allowedModes = map[string]bool{}
	for _, m := range detectBlendModes() {
		allowedModes[m] = true
	}
	if !allowedModes["screen"] {
		allowedModes["screen"] = true
	}

	if err := os.MkdirAll(jobsRoot, 0o755); err != nil {
		log.Fatalf("cannot create %s: %v", jobsRoot, err)
	}

	a := newApp()
	go a.janitor()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleIndex)
	mux.HandleFunc("GET /api/modes", handleModes)
	mux.HandleFunc("POST /api/upload", a.handleUpload)
	mux.HandleFunc("POST /api/generate/{id}", a.handleGenerate)
	mux.HandleFunc("GET /api/job/{id}", a.handleJobStatus)
	mux.HandleFunc("GET /api/result/{id}", a.handleResult)

	log.Printf("listening on %s — open http://localhost%s/", listenAddr, listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
