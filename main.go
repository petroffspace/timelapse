package main

import (
	"bufio"
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
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed static/index.html
var indexHTML []byte

const (
	listenAddr        = ":9595"
	maxUploadBytes    = 2 << 30 // 2 GiB
	maxProcessTime    = 2 * time.Hour
	jobTTL            = 2 * time.Hour
	resultName        = "result.mp4"
	maxConcurrentJobs = 2
	maxChunk          = 120
	maxSaneFPS        = 240
	probeTimeout      = 30 * time.Second
	jobsRoot          = "jobs"
	fallbackModesStr  = "addition addition128 and average bleach burn darken difference divide dodge extremity exclusion freeze glow grainextract grainmerge hardlight hardmix heat lighten linearlight multiply multiply128 negation normal or overlay phoenix pinlight reflect screen softlight subtract vividlight xor"
)

var (
	idRe         = regexp.MustCompile(`^[0-9a-f]{12}$`)
	modeTokenRe  = regexp.MustCompile(`^[a-z0-9]+$`)
	allowedModes = map[string]bool{}
)

// ============================================================ job model

type Status string

const (
	StatusUploaded   Status = "uploaded"
	StatusQueued     Status = "queued"
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
	EstFrames   int
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
	log.Printf("job %s DONE: %d blended groups", j.ID, j.GroupsDone)
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
	j.mux.Lock()
	defer j.mux.Unlock()
	return j.Status, j.FinishedAt
}

// ============================================================ store

type store struct {
	mu sync.RWMutex
	m  map[string]*Job
}

func newStore() *store { return &store{m: map[string]*Job{}} }

func (s *store) add(j *Job) { s.mu.Lock(); s.m[j.ID] = j; s.mu.Unlock() }
func (s *store) get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.m[id]
	return j, ok
}
func (s *store) purge(id string) { s.mu.Lock(); delete(s.m, id); s.mu.Unlock() }

// ============================================================ app

type app struct {
	ctx   context.Context // cancelled on shutdown; aborts running jobs
	store *store
	sem   chan struct{}
}

func newApp(ctx context.Context) *app {
	return &app{
		ctx:   ctx,
		store: newStore(),
		sem:   make(chan struct{}, maxConcurrentJobs),
	}
}

func (a *app) jobDir(id string) string { return filepath.Join(jobsRoot, id) }

// ============================================================ ffmpeg

// runFFmpeg runs ffmpeg with machine-readable progress on stdout and calls
// onFrame with the number of output frames written so far.
func runFFmpeg(ctx context.Context, onFrame func(int), args ...string) error {
	full := append([]string{"-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:1"}, args...)
	cmd := exec.CommandContext(ctx, "ffmpeg", full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start ffmpeg: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "frame="); ok && onFrame != nil {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				onFrame(n)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg aborted: %w", ctx.Err())
		}
		snippet := stderr.String()
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

// parseRational parses "num/den" (or a bare integer) into positive integers.
func parseRational(s string) (num, den int64, ok bool) {
	s = strings.TrimSpace(s)
	ns, ds, found := strings.Cut(s, "/")
	if !found {
		ds = "1"
	}
	n, err1 := strconv.ParseInt(ns, 10, 64)
	d, err2 := strconv.ParseInt(ds, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0, 0, false
	}
	return n, d, true
}

// saneFPS reports whether s is a usable frame-rate fraction. Containers often
// report bogus r_frame_rate values (e.g. 90000/1) for VFR streams, which would
// make the fps filter emit a flood of duplicated frames.
func saneFPS(s string) (float64, bool) {
	n, d, ok := parseRational(s)
	if !ok {
		return 0, false
	}
	fps := float64(n) / float64(d)
	return fps, fps <= maxSaneFPS
}

type probeInfo struct {
	FPSFraction string
	FPS         float64
	Duration    float64
}

func probeVideo(ctx context.Context, path string) (probeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate,avg_frame_rate,duration:format=duration",
		"-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return probeInfo{}, fmt.Errorf("ffprobe failed: %w", err)
	}
	var parsed struct {
		Streams []struct {
			RFrameRate   string `json:"r_frame_rate"`
			AvgFrameRate string `json:"avg_frame_rate"`
			Duration     string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Streams) == 0 {
		return probeInfo{}, errors.New("no video stream found or unreadable ffprobe output")
	}
	st := parsed.Streams[0]
	frac := "25/1"
	for _, cand := range []string{st.RFrameRate, st.AvgFrameRate} {
		if _, ok := saneFPS(cand); ok {
			frac = cand
			break
		}
	}
	fps, _ := saneFPS(frac)
	dur, err := strconv.ParseFloat(st.Duration, 64)
	if err != nil || dur <= 0 {
		dur, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	}
	return probeInfo{FPSFraction: frac, FPS: fps, Duration: dur}, nil
}

// buildFilterGraph blends every consecutive `chunk` frames into one output
// frame in a single ffmpeg pass. The stream is split into `chunk` branches;
// branch k keeps frames with n%chunk == k and is retimed so that all frames of
// group g share timestamp g, letting the blend chain pair them up. For a short
// final group, eof_action=pass forwards the partial blend unchanged.
//
// format=rgb24 before gbrp mirrors the former PNG-intermediate pipeline
// (blend runs on planar RGB), keeping results bit-identical to it.
//
// "average" uses tmix instead: chaining pairwise blend=average halves the
// weight of earlier frames at every step, whereas tmix gives every frame of
// the group equal weight. See dropsPartialGroup for the tail caveat.
func buildFilterGraph(fpsFrac string, chunk int, mode string) (string, error) {
	num, den, ok := parseRational(fpsFrac)
	if !ok {
		return "", fmt.Errorf("invalid frame rate %q", fpsFrac)
	}
	const encodePrep = "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p"
	var fc strings.Builder
	fmt.Fprintf(&fc, "[0:v]fps=%d/%d,format=rgb24,format=gbrp", num, den)
	if chunk <= 1 {
		fc.WriteString("," + encodePrep + "[out]")
		return fc.String(), nil
	}
	if dropsPartialGroup(mode, chunk) {
		// tmix emits a sliding-window mean per input frame; keep only the frame
		// that closes each group, when the window spans exactly that group.
		fmt.Fprintf(&fc, `,tmix=frames=%d,select='eq(mod(n\,%d)\,%d)',settb=%d/%d,setpts=N,%s[out]`,
			chunk, chunk, chunk-1, den, num, encodePrep)
		return fc.String(), nil
	}
	fmt.Fprintf(&fc, ",split=%d", chunk)
	for k := 0; k < chunk; k++ {
		fmt.Fprintf(&fc, "[s%d]", k)
	}
	fc.WriteByte(';')
	for k := 0; k < chunk; k++ {
		fmt.Fprintf(&fc, `[s%d]select='eq(mod(n\,%d)\,%d)',settb=%d/%d,setpts=N[t%d];`, k, chunk, k, den, num, k)
	}
	prev := "t0"
	for k := 1; k < chunk; k++ {
		fmt.Fprintf(&fc, "[%s][t%d]blend=all_mode=%s:eof_action=pass[b%d];", prev, k, mode, k)
		prev = fmt.Sprintf("b%d", k)
	}
	fmt.Fprintf(&fc, "[%s]%s[out]", prev, encodePrep)
	return fc.String(), nil
}

// dropsPartialGroup reports whether a short final group is discarded. tmix's
// window always spans `chunk` frames and select can't detect end of stream,
// so for "average" the trailing frames (< chunk, under ~1s of source) that
// don't fill a whole group produce no output frame.
func dropsPartialGroup(mode string, chunk int) bool {
	return mode == "average" && chunk > 1
}

// ============================================================ pipeline

func process(ctx context.Context, j *Job, a *app) {
	dir := a.jobDir(j.ID)
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir) // failed jobs cannot be retried; free the disk now
		}
	}()

	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		j.fail(errors.New("cancelled"))
		return
	}

	ctx, cancel := context.WithTimeout(ctx, maxProcessTime)
	defer cancel()

	j.mux.Lock()
	j.Status = StatusProcessing
	mode, chunk, estFrames := j.BlendMode, j.Chunk, j.EstFrames
	j.mux.Unlock()

	graph, err := buildFilterGraph(j.FPSFraction, chunk, mode)
	if err != nil {
		j.fail(err)
		return
	}

	dropTail := dropsPartialGroup(mode, chunk)
	estGroups := (estFrames + chunk - 1) / chunk
	if dropTail {
		estGroups = estFrames / chunk
	}
	j.mutate(func(jj *Job) { jj.TotalFrames = estFrames; jj.TotalGroups = estGroups })
	j.setStage("blending_and_encoding", 0)

	inputPath := filepath.Join(dir, j.InputName)
	onFrame := func(n int) {
		j.mutate(func(jj *Job) {
			jj.GroupsDone = n
			if n > jj.TotalGroups {
				jj.TotalGroups = n
			}
			if jj.TotalGroups > 0 {
				jj.Progress = math.Min(99, 100*float64(n)/float64(jj.TotalGroups))
			}
		})
	}
	if err := runFFmpeg(ctx, onFrame,
		"-i", inputPath,
		"-filter_complex", graph,
		"-map", "[out]",
		"-fps_mode", "passthrough",
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", "18",
		"-movflags", "+faststart",
		"-an",
		filepath.Join(dir, resultName),
	); err != nil {
		j.fail(fmt.Errorf("processing failed: %w", err))
		return
	}

	j.mux.Lock()
	groups := j.GroupsDone
	j.mux.Unlock()
	if groups == 0 {
		if dropTail {
			j.fail(fmt.Errorf("video too short: average mode needs at least %d frames", chunk))
			return
		}
		j.fail(errors.New("no frames produced"))
		return
	}
	// Exact input frame count isn't reported; clamp the estimate into the
	// range consistent with the number of groups actually produced.
	j.mutate(func(jj *Job) {
		jj.TotalGroups = groups
		lo, hi := (groups-1)*chunk+1, groups*chunk
		if dropTail {
			lo, hi = groups*chunk, (groups+1)*chunk-1
		}
		jj.TotalFrames = max(min(jj.TotalFrames, hi), lo)
	})

	ok = true
	os.Remove(inputPath) // only the result is served from here on
	j.finishOK()
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
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

func sanitizeExtension(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mov", ".mkv", ".avi", ".webm", ".m4v", ".mpg", ".mpeg", ".ts":
		return ext
	default:
		return ".bin"
	}
}

// saveUpload streams the "video" multipart field straight into dir, avoiding
// the temp-file copy that ParseMultipartForm makes for large uploads.
func saveUpload(r *http.Request, dir string) (name string, size int64, status int, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", 0, http.StatusBadRequest, errors.New("expected multipart/form-data")
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", 0, http.StatusBadRequest, errors.New(`missing field "video"`)
		}
		if err != nil {
			return "", 0, uploadErrStatus(err), errors.New("upload too large or malformed")
		}
		if part.FormName() != "video" {
			part.Close()
			continue
		}
		name = "input" + sanitizeExtension(part.FileName())
		dst, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return "", 0, http.StatusInternalServerError, errors.New("cannot write file")
		}
		size, err = io.Copy(dst, part)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", 0, uploadErrStatus(err), errors.New("failed saving upload")
		}
		return name, size, 0, nil
	}
}

func uploadErrStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	id := newID()
	dir := a.jobDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "cannot create job directory")
		return
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()

	inName, size, code, err := saveUpload(r, dir)
	if err != nil {
		jsonErr(w, code, err.Error())
		return
	}
	if size == 0 {
		jsonErr(w, http.StatusBadRequest, "empty file")
		return
	}

	info, err := probeVideo(r.Context(), filepath.Join(dir, inName))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "not a readable video: "+err.Error())
		return
	}

	chunk := min(max(int(math.Round(info.FPS)), 1), maxChunk)

	j := &Job{
		ID: id, Status: StatusUploaded, Stage: "awaiting_generate", CreatedAt: time.Now(),
		FPSFraction: info.FPSFraction, FPS: math.Round(info.FPS*1000) / 1000, Chunk: chunk, InputName: inName,
		EstFrames: int(math.Round(info.Duration * info.FPS)),
	}
	keep = true
	a.store.add(j)
	log.Printf("upload %s: %.1f MiB, fps=%s (%.3f), %.1fs -> chunk=%d", id, float64(size)/(1<<20), info.FPSFraction, info.FPS, info.Duration, chunk)

	writeJSON(w, http.StatusOK, j.dto())
}

// lookupJob resolves the {id} path value, writing a 404 if it is unknown.
func (a *app) lookupJob(w http.ResponseWriter, r *http.Request) (*Job, bool) {
	id := r.PathValue("id")
	if idRe.MatchString(id) {
		if j, ok := a.store.get(id); ok {
			return j, true
		}
	}
	jsonErr(w, http.StatusNotFound, "unknown job")
	return nil, false
}

func (a *app) handleGenerate(w http.ResponseWriter, r *http.Request) {
	j, ok := a.lookupJob(w, r)
	if !ok {
		return
	}

	var body struct {
		Mode string `json:"mode"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Mode == "" {
		jsonErr(w, http.StatusBadRequest, `missing "mode" in body`)
		return
	}
	mode := strings.ToLower(body.Mode)
	if !allowedModes[mode] {
		jsonErr(w, http.StatusBadRequest, "unsupported blend mode: "+body.Mode)
		return
	}

	// Check-and-transition atomically so concurrent requests can't start the
	// same job twice.
	j.mux.Lock()
	st := j.Status
	if st == StatusUploaded {
		j.Status, j.Stage, j.BlendMode = StatusQueued, "queued", mode
	}
	j.mux.Unlock()
	switch st {
	case StatusUploaded:
	case StatusQueued, StatusProcessing:
		jsonErr(w, http.StatusConflict, "already processing")
		return
	case StatusDone:
		jsonErr(w, http.StatusConflict, "already finished")
		return
	default:
		jsonErr(w, http.StatusConflict, "job previously failed")
		return
	}

	go process(a.ctx, j, a)
	writeJSON(w, http.StatusAccepted, j.dto())
}

func (a *app) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	if j, ok := a.lookupJob(w, r); ok {
		writeJSON(w, http.StatusOK, j.dto())
	}
}

func (a *app) handleResult(w http.ResponseWriter, r *http.Request) {
	j, ok := a.lookupJob(w, r)
	if !ok {
		return
	}
	if st, _ := j.statusSnapshot(); st != StatusDone {
		jsonErr(w, http.StatusConflict, "result not ready")
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="timelapse_`+j.ID+`.mp4"`)
	}
	http.ServeFile(w, r, filepath.Join(a.jobDir(j.ID), resultName))
}

func handleModes(modesJSON []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(modesJSON)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// ============================================================ janitor

func (a *app) janitor() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		cutoff := time.Now().Add(-jobTTL)
		for _, j := range a.store.list() {
			st, fin := j.statusSnapshot()
			expired := (st == StatusDone || st == StatusFailed) && fin.Before(cutoff) ||
				st == StatusUploaded && j.CreatedAt.Before(cutoff)
			if expired {
				os.RemoveAll(a.jobDir(j.ID))
				a.store.purge(j.ID)
				log.Printf("janitor: expired %s job %s", st, j.ID)
			}
		}
	}
}

func (s *store) list() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]*Job, 0, len(s.m))
	for _, j := range s.m {
		jobs = append(jobs, j)
	}
	return jobs
}

// removeStaleJobDirs deletes job directories left over from a previous run;
// the job store is in-memory, so they can never be served again.
func removeStaleJobDirs() {
	entries, err := os.ReadDir(jobsRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && idRe.MatchString(e.Name()) {
			os.RemoveAll(filepath.Join(jobsRoot, e.Name()))
			log.Printf("removed stale job dir %s", e.Name())
		}
	}
}

// ============================================================ main

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	modes := detectBlendModes()
	for _, m := range modes {
		allowedModes[m] = true
	}
	modesJSON, _ := json.Marshal(map[string]any{"modes": modes})

	if err := os.MkdirAll(jobsRoot, 0o755); err != nil {
		log.Fatalf("cannot create %s: %v", jobsRoot, err)
	}
	removeStaleJobDirs()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := newApp(ctx)
	go a.janitor()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /api/modes", handleModes(modesJSON))
	mux.HandleFunc("POST /api/upload", a.handleUpload)
	mux.HandleFunc("POST /api/generate/{id}", a.handleGenerate)
	mux.HandleFunc("GET /api/job/{id}", a.handleJobStatus)
	mux.HandleFunc("GET /api/result/{id}", a.handleResult)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()

	log.Printf("listening on %s — open http://localhost%s/", listenAddr, listenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
