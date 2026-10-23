package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// journalctl -o cat keeps the ANSI colour codes from xochitl's log lines.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// sleepWatcher follows xochitl's display state ("Normal" / "DeepSleep").
//
// A Paper Pro connected to USB never really suspends (udev.charger holds a
// wake lock), so processes and Wi-Fi keep running after the screen goes to
// sleep. The only reliable signal is xochitl's log line
// "Changing display state from Normal to DeepSleep". While the tablet sleeps,
// the proxy rejects streaming requests (503) and drops active connections.
//
// The same log also carries the UI orientation ("Setting new orientation
// Landscape"), so the browser preview can rotate together with the tablet.
type sleepWatcher struct {
	mu     sync.Mutex
	awake  bool
	since  time.Time
	ctx    context.Context // cancelled when the tablet goes to sleep
	cancel context.CancelFunc

	orientation string        // Portrait / Landscape / InvertedLandscape / InvertedPortrait
	version     uint64        // incremented on every state change (awake or orientation)
	changed     chan struct{} // closed and replaced on every change (/state long-poll)
}

func newSleepWatcher() *sleepWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &sleepWatcher{
		awake: true, since: time.Now(), ctx: ctx, cancel: cancel,
		orientation: "Portrait", version: 1, changed: make(chan struct{}),
	}
}

// bumpLocked announces a state change to waiting long-polls. Requires s.mu.
func (s *sleepWatcher) bumpLocked() {
	s.version++
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *sleepWatcher) setOrientation(o string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o == s.orientation {
		return
	}
	log.Printf("tablet orientation: %s", o)
	s.orientation = o
	s.bumpLocked()
}

func (s *sleepWatcher) isAwake() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.awake
}

// awakeContext returns the context of the current awake period; it is
// cancelled when the tablet goes to sleep. Grab it at the start of a request.
func (s *sleepWatcher) awakeContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

func (s *sleepWatcher) set(awake bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if awake == s.awake {
		return
	}
	s.awake = awake
	s.since = time.Now()
	s.bumpLocked()
	if awake {
		log.Printf("tablet awake: streaming allowed")
		return
	}
	log.Printf("tablet asleep: dropping streams, streaming blocked")
	s.cancel()
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

// parseDisplayState extracts the new state from a xochitl log line.
// It returns (state, true), or ("", false) when the line is not about the
// display state.
func parseDisplayState(line string) (string, bool) {
	const marker = "Changing display state from "
	line = ansiEscape.ReplaceAllString(line, "")
	i := strings.Index(line, marker)
	if i < 0 {
		return "", false
	}
	f := strings.Fields(line[i+len(marker):])
	if len(f) < 3 || f[1] != "to" {
		return "", false
	}
	return f[2], true
}

// parseOrientation extracts the UI orientation from a xochitl log line:
//
//	rm.orientationmodule  Setting new orientation Landscape (setOrientation ...)
//	rm.orientationmodule  Setting forced portrait window orientation Portrait (...)
func parseOrientation(line string) (string, bool) {
	line = ansiEscape.ReplaceAllString(line, "")
	for _, marker := range []string{"Setting new orientation ", "Setting forced portrait window orientation "} {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		f := strings.Fields(line[i+len(marker):])
		if len(f) == 0 {
			return "", false
		}
		switch f[0] {
		case "Portrait", "Landscape", "InvertedLandscape", "InvertedPortrait":
			return f[0], true
		}
		return "", false
	}
	return "", false
}

// handleLine updates the state from one xochitl log line.
func (s *sleepWatcher) handleLine(line string) {
	if st, ok := parseDisplayState(line); ok {
		s.set(st == "Normal")
	}
	if o, ok := parseOrientation(line); ok {
		s.setOrientation(o)
	}
}

// run follows the xochitl journal; on error it retries every few seconds.
func (s *sleepWatcher) run(unit string) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		log.Printf("sleep-watch: journalctl not found, assuming the tablet is always awake")
		return
	}
	for {
		s.syncInitialState(unit)
		cmd := exec.Command("journalctl", "-u", unit, "-f", "-n", "0", "-o", "cat")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("sleep-watch: journalctl: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			s.handleLine(sc.Text())
		}
		_ = cmd.Wait()
		log.Printf("sleep-watch: journalctl exited, restarting")
		time.Sleep(5 * time.Second)
	}
}

// syncInitialState derives the state from the last matching entries since boot.
func (s *sleepWatcher) syncInitialState(unit string) {
	out, err := exec.Command("journalctl", "-u", unit, "-b", "--no-pager", "-o", "cat").Output()
	if err != nil {
		return
	}
	state, orientation := "Normal", "Portrait"
	for _, line := range strings.Split(string(out), "\n") {
		if st, ok := parseDisplayState(line); ok {
			state = st
		}
		if o, ok := parseOrientation(line); ok {
			orientation = o
		}
	}
	s.set(state == "Normal")
	s.setOrientation(orientation)
}

// isStreamPath reports which upstream paths carry screen images or events.
func isStreamPath(p string) bool {
	switch p {
	case "/stream", "/screenshot", "/events", "/gestures", "/raw":
		return true
	}
	return false
}

// gateSleep blocks streaming paths while the tablet sleeps, and drops active
// requests the moment it falls asleep.
func gateSleep(sw *sleepWatcher, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStreamPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !sw.isAwake() {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-RM-State", "asleep")
			http.Error(w, "asleep", http.StatusServiceUnavailable)
			return
		}
		awakeCtx := sw.awakeContext()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-awakeCtx.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// handleState returns the tablet state. With ?wait=<version> it acts as a
// long-poll: if the version has not changed, it waits up to 25 s for a change
// (orientation or sleep), so the browser learns about a rotation right away.
// It also tells the page the viewer's role and whether it may draw.
func handleState(sw *sleepWatcher, vs *viewerSignal, penEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("wait"); v != "" {
			var want uint64
			fmt.Sscan(v, &want)
			sw.mu.Lock()
			cur, ch := sw.version, sw.changed
			sw.mu.Unlock()
			if want == cur {
				t := time.NewTimer(25 * time.Second)
				select {
				case <-ch:
				case <-t.C:
				case <-r.Context().Done():
					t.Stop()
					return
				}
				t.Stop()
			}
		}
		sw.mu.Lock()
		resp := map[string]any{
			"awake":       sw.awake,
			"since":       sw.since.UTC().Format(time.RFC3339),
			"orientation": sw.orientation,
			"version":     sw.version,
		}
		sw.mu.Unlock()
		if vs != nil {
			resp["viewers"] = vs.viewers()
		}
		if sess, ok := sessionFrom(r); ok {
			resp["role"] = sess.Role
			resp["canDraw"] = penEnabled && sess.canDraw()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
