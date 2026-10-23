package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// Drawing jobs: the Mermaid import in the browser (client_mermaid.patch)
// renders a diagram, lays its labels out on the lines of xochitl's text and
// sends the result here in two steps:
//
//  1. the labels, as text typed with the virtual keyboard (keyboard.go);
//  2. once it has found that text on a screenshot, the boxes and arrows, as
//     strokes drawn with the remote pen (pen.go), paced like a real pen.
//
// Text goes first because xochitl anchors strokes to the text: typing above
// existing strokes pushes them down.
//
// Endpoints (owner only):
//
//	GET  /draw/info   {"chars": "<characters the keyboard can type>", "keyboard": true}
//	GET  /draw/font   the font xochitl uses for typed text
//	POST /draw        {"text": "...", "pagesUp": n, "strokes": [[x0, y0, x1, y1, ...], ...]}
//
// Each part is optional and they run in this order. The text is typed as a
// new paragraph after the page's text: "\n" starts another paragraph, U+2028
// (LINE SEPARATOR) another line of the same paragraph; typing leaves the page
// scrolled to the end of the text. pagesUp presses Page Up n times, 80 % of
// the screen each: a diagram taller than the screen is drawn in parts from
// the bottom up, scrolling in between. Stroke coordinates are normalised to 0..1 of
// the screen as it is (native portrait), like the /pen protocol. POST /draw
// streams its progress as JSON lines ({"phase": "typing"|"waiting"|"drawing",
// "done": n, "total": m}) and ends with {"ok": true} or {"error": "..."}. Closing the
// request stops the job.
const (
	penPointInterval = 4 * time.Millisecond  // between pen samples (a real pen reports at a few hundred Hz)
	penRest          = 20 * time.Millisecond // still before lifting, or xochitl clips the end of the stroke
	penStrokeGap     = 40 * time.Millisecond // between strokes
	leadInSamples    = 20                    // hovering approach before each stroke...
	leadInStep       = 0.0018                // ...in steps of about 3 px (normalised width units)
	maxDrawText      = 20000                 // characters per job
	maxDrawPoints    = 200000                // pen samples per job
	maxPagesUp       = 100
	idleTimeout      = 2 * time.Minute
	idleTicks        = 10 // xochitl's CPU time per 0.5 s (in 1/100 s) below which it is idle: a fifth of a core
	maxDrawBody      = 8 << 20
)

// textFontName is the font xochitl uses for typed body text.
const textFontName = "reMarkable Sans Regular"

type drawJob struct {
	Text    string      `json:"text"`
	PagesUp *int        `json:"pagesUp"`
	Strokes [][]float64 `json:"strokes"`
}

func (j *drawJob) validate() error {
	if len([]rune(j.Text)) > maxDrawText {
		return fmt.Errorf("text too long (max %d characters)", maxDrawText)
	}
	if _, err := keystrokes(j.Text); err != nil {
		return err
	}
	if j.PagesUp != nil && (*j.PagesUp < 0 || *j.PagesUp > maxPagesUp) {
		return errors.New("pagesUp out of range")
	}
	points := 0
	for _, s := range j.Strokes {
		if len(s) < 2 || len(s)%2 != 0 {
			return errors.New("a stroke needs x,y pairs")
		}
		for _, v := range s {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("invalid coordinate")
			}
		}
		points += len(s) / 2
	}
	if points > maxDrawPoints {
		return fmt.Errorf("too many points (max %d)", maxDrawPoints)
	}
	return nil
}

type drawer struct {
	pen        *penWriter
	kb         *virtualKeyboard // nil = typing disabled
	sw         *sleepWatcher
	xochitlBin string
	busy       atomic.Bool

	fontOnce sync.Once
	font     []byte
}

func newDrawer(pen *penWriter, kb *virtualKeyboard, sw *sleepWatcher, xochitlBin string) *drawer {
	return &drawer{pen: pen, kb: kb, sw: sw, xochitlBin: xochitlBin}
}

func (d *drawer) awake() bool { return d.sw == nil || d.sw.isAwake() }

func (d *drawer) info(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"chars":    typeableChars(),
		"keyboard": d.kb != nil,
	})
}

// textFont serves reMarkable Sans Regular so that the browser measures labels
// exactly like xochitl lays them out. The font is embedded in the xochitl
// binary; it is looked up there instead of being shipped with this project.
func (d *drawer) textFont(w http.ResponseWriter, r *http.Request) {
	d.fontOnce.Do(func() {
		data, err := os.ReadFile(d.xochitlBin)
		if err != nil {
			log.Printf("draw: %v", err)
			return
		}
		if f := findFont(data, textFontName); f != nil {
			d.font = bytes.Clone(f)
		} else {
			log.Printf("draw: %q not found in %s", textFontName, d.xochitlBin)
		}
	})
	if d.font == nil {
		http.Error(w, "the text font was not found on the tablet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "font/ttf")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(d.font)
}

func (d *drawer) run(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var job drawJob
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDrawBody)).Decode(&job); err != nil {
		http.Error(w, "invalid job: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := job.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if (job.Text != "" || job.PagesUp != nil) && d.kb == nil {
		http.Error(w, "typing is disabled (-keyboard-device)", http.StatusServiceUnavailable)
		return
	}
	if !d.awake() {
		http.Error(w, "the tablet is asleep", http.StatusServiceUnavailable)
		return
	}
	if !d.busy.CompareAndSwap(false, true) {
		http.Error(w, "another drawing job is running", http.StatusConflict)
		return
	}
	defer d.busy.Store(false)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	var last time.Time
	var lastPhase string
	progress := func(phase string) func(done, total int) {
		return func(done, total int) {
			if phase != lastPhase || done == total || time.Since(last) > 250*time.Millisecond {
				last, lastPhase = time.Now(), phase
				send(map[string]any{"phase": phase, "done": done, "total": total})
			}
		}
	}

	up := "-"
	if job.PagesUp != nil {
		up = fmt.Sprint(*job.PagesUp)
	}
	log.Printf("draw: job from %s: %d characters, %s pages up, %d strokes", clientIP(r), len([]rune(job.Text)), up, len(job.Strokes))
	err := d.execute(r.Context(), &job, progress)
	if err != nil {
		log.Printf("draw: %v", err)
		send(map[string]string{"error": err.Error()})
		return
	}
	send(map[string]bool{"ok": true})
}

func (d *drawer) execute(ctx context.Context, job *drawJob, progress func(phase string) func(done, total int)) error {
	if job.Text != "" {
		if err := d.kb.typeText(ctx, job.Text, progress("typing")); err != nil {
			return err
		}
	}
	if job.PagesUp != nil {
		if err := d.kb.scrollUp(ctx, *job.PagesUp); err != nil {
			return err
		}
	}
	// xochitl lays typed text out much more slowly than the keys arrive,
	// and drops pen input while it is busy: wait until it has caught up.
	progress("waiting")(0, 1)
	if err := waitIdle(ctx); err != nil {
		return err
	}
	if len(job.Strokes) == 0 {
		return nil
	}
	owner := new(int)
	if !d.pen.reserve(owner) {
		return errors.New("someone is drawing on the tablet")
	}
	defer d.pen.release(owner)
	total := 0
	for _, s := range job.Strokes {
		total += len(s) / 2
	}
	report := progress("drawing")
	done := 0
	for _, s := range job.Strokes {
		if !d.awake() {
			return errors.New("the tablet fell asleep")
		}
		for _, p := range leadIn(s) {
			d.pen.apply(owner, 'h', p[0], p[1])
			if err := sleepCtx(ctx, penPointInterval); err != nil {
				return err
			}
		}
		for i := 0; i < len(s); i += 2 {
			cmd := byte('m')
			if i == 0 {
				cmd = 'd'
			}
			if !d.pen.apply(owner, cmd, s[i], s[i+1]) {
				return errors.New("cannot use the pen device")
			}
			done++
			report(done, total)
			if err := sleepCtx(ctx, penPointInterval); err != nil {
				return err
			}
		}
		// Rest at the end point, then lift.
		d.pen.apply(owner, 'm', s[len(s)-2], s[len(s)-1])
		if err := sleepCtx(ctx, penRest); err != nil {
			return err
		}
		d.pen.apply(owner, 'u', 0, 0)
		if err := sleepCtx(ctx, penStrokeGap); err != nil {
			return err
		}
	}
	return nil
}

// leadIn returns the hover positions that approach the start of stroke s
// along its first direction. xochitl smooths pen input with some inertia: a
// pen that touches down at rest starts every stroke with a wiggle, one that
// arrives moving, like a hand, does not. Coordinates are normalised and a
// screen is about 3:4, so a vertical unit is 4/3 of a horizontal one.
func leadIn(s []float64) [][2]float64 {
	x0, y0 := s[0], s[1]
	dx, dy := 0.0, 1.0 // a dot: come from above
	for i := 2; i+1 < len(s); i += 2 {
		if ex, ey := s[i]-x0, (s[i+1]-y0)*4/3; math.Hypot(ex, ey) > 0.002 {
			dx, dy = ex, ey
			break
		}
	}
	l := math.Hypot(dx, dy)
	out := make([][2]float64, 0, leadInSamples)
	for i := leadInSamples; i > 0; i-- {
		d := float64(i) * leadInStep / l
		out = append(out, [2]float64{clamp01(x0 - dx*d), clamp01(y0 - dy*d*3/4)})
	}
	return out
}

// waitIdle returns once xochitl has used less than idleTicks of CPU time in
// two half-second periods in a row, or after idleTimeout.
func waitIdle(ctx context.Context) error {
	pid := findProcess("xochitl")
	if pid == "" {
		return nil
	}
	prev, ok := cpuTicks(pid)
	quiet := 0
	for deadline := time.Now().Add(idleTimeout); ok && time.Now().Before(deadline); {
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return err
		}
		var cur int64
		if cur, ok = cpuTicks(pid); !ok {
			break
		}
		if cur-prev < idleTicks {
			quiet++
		} else {
			quiet = 0
		}
		if quiet == 2 {
			return nil
		}
		prev = cur
	}
	log.Printf("draw: xochitl did not become idle")
	return nil
}

// findProcess returns the PID of the process called name, or "".
func findProcess(name string) string {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if comm, err := os.ReadFile("/proc/" + e.Name() + "/comm"); err == nil && strings.TrimSpace(string(comm)) == name {
			return e.Name()
		}
	}
	return ""
}

// cpuTicks returns the CPU time a process has used (user + system, in
// clock ticks), from /proc/<pid>/stat.
func cpuTicks(pid string) (int64, bool) {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return 0, false
	}
	return parseCPUTicks(string(b))
}

func parseCPUTicks(stat string) (int64, bool) {
	// The command name in parentheses may contain spaces; fields 14 and 15
	// (utime, stime) follow it.
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	s, err2 := strconv.ParseInt(f[12], 10, 64)
	return u + s, err1 == nil && err2 == nil
}

// ---------------------------------------------------------------------------
// Fonts embedded in a binary
// ---------------------------------------------------------------------------

// findFont returns the TrueType font called fullName that is stored
// uncompressed somewhere in data, or nil.
func findFont(data []byte, fullName string) []byte {
	sig := []byte{0, 1, 0, 0} // sfnt version 1.0
	for off := 0; ; {
		i := bytes.Index(data[off:], sig)
		if i < 0 {
			return nil
		}
		start := off + i
		off = start + 1
		if f := sfntAt(data[start:]); f != nil && sfntFullName(f) == fullName {
			return f
		}
	}
}

// sfntAt returns the font that starts at b, or nil when b does not start
// with a plausible table directory.
func sfntAt(b []byte) []byte {
	if len(b) < 12 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(b[4:]))
	dirEnd := 12 + 16*n
	if n < 4 || n > 64 || len(b) < dirEnd {
		return nil
	}
	end := dirEnd
	for t := 0; t < n; t++ {
		rec := b[12+16*t:]
		for _, c := range rec[:4] {
			if c < 0x20 || c > 0x7e {
				return nil
			}
		}
		off, size := int(binary.BigEndian.Uint32(rec[8:])), int(binary.BigEndian.Uint32(rec[12:]))
		if off < dirEnd || size < 0 || off+size > len(b) {
			return nil
		}
		end = max(end, off+size)
	}
	return b[:end]
}

// sfntFullName returns the full font name (name ID 4, Windows platform).
func sfntFullName(f []byte) string {
	n := int(binary.BigEndian.Uint16(f[4:]))
	for t := 0; t < n; t++ {
		rec := f[12+16*t:]
		if string(rec[:4]) != "name" {
			continue
		}
		off, size := int(binary.BigEndian.Uint32(rec[8:])), int(binary.BigEndian.Uint32(rec[12:]))
		tab := f[off : off+size]
		if len(tab) < 6 {
			return ""
		}
		count, strOff := int(binary.BigEndian.Uint16(tab[2:])), int(binary.BigEndian.Uint16(tab[4:]))
		for i := 0; i < count && 6+12*i+12 <= len(tab); i++ {
			r := tab[6+12*i:]
			platform, nameID := binary.BigEndian.Uint16(r[0:]), binary.BigEndian.Uint16(r[6:])
			length, at := int(binary.BigEndian.Uint16(r[8:])), strOff+int(binary.BigEndian.Uint16(r[10:]))
			if platform != 3 || nameID != 4 || at+length > len(tab) {
				continue
			}
			u := make([]uint16, length/2)
			for k := range u {
				u[k] = binary.BigEndian.Uint16(tab[at+2*k:])
			}
			return string(utf16.Decode(u))
		}
	}
	return ""
}
