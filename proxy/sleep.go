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

// journal -o cat zachowuje kody kolorów ANSI z logów xochitl.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// sleepWatcher śledzi stan wyświetlacza xochitl ("Normal" / "DeepSleep").
//
// Paper Pro na kablu USB nie wchodzi w suspend (blokada udev.charger), więc po
// uśpieniu ekranu procesy i Wi-Fi dalej działają. Jedynym wiarygodnym sygnałem
// jest log xochitl: "Changing display state from Normal to DeepSleep".
// Gdy tablet śpi, proxy odrzuca strumień (503) i zrywa aktywne połączenia.
//
// Z tego samego logu czyta też orientację interfejsu ("Setting new orientation
// Landscape"), żeby podgląd w przeglądarce obracał się razem z tabletem.
type sleepWatcher struct {
	mu     sync.Mutex
	awake  bool
	since  time.Time
	ctx    context.Context // anulowany przy przejściu w sen
	cancel context.CancelFunc

	orientation string        // Portrait / Landscape / InvertedLandscape / InvertedPortrait
	version     uint64        // rośnie przy każdej zmianie stanu (awake lub orientacja)
	changed     chan struct{} // zamykany i podmieniany przy każdej zmianie (long-poll /state)
}

func newSleepWatcher() *sleepWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &sleepWatcher{
		awake: true, since: time.Now(), ctx: ctx, cancel: cancel,
		orientation: "Portrait", version: 1, changed: make(chan struct{}),
	}
}

// bumpLocked ogłasza zmianę stanu czekającym long-pollom. Wymaga s.mu.
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
	log.Printf("orientacja tabletu: %s", o)
	s.orientation = o
	s.bumpLocked()
}

func (s *sleepWatcher) isAwake() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.awake
}

// awakeContext zwraca kontekst bieżącego okresu czuwania; zostaje anulowany,
// gdy tablet zaśnie. Pobierz go na początku żądania.
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
		log.Printf("tablet obudzony: streaming dozwolony")
		return
	}
	log.Printf("tablet uśpiony: zrywam strumienie, streaming zablokowany")
	s.cancel()
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

// parseDisplayState wyciąga nowy stan z linii logu xochitl.
// Zwraca (stan, true) albo ("", false), gdy linia nie dotyczy stanu wyświetlacza.
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

// parseOrientation wyciąga orientację interfejsu z linii logu xochitl:
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

// handleLine aktualizuje stan na podstawie jednej linii logu xochitl.
func (s *sleepWatcher) handleLine(line string) {
	if st, ok := parseDisplayState(line); ok {
		s.set(st == "Normal")
	}
	if o, ok := parseOrientation(line); ok {
		s.setOrientation(o)
	}
}

// run śledzi journal xochitl; przy błędzie próbuje ponownie co kilka sekund.
func (s *sleepWatcher) run(unit string) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		log.Printf("sleep-watch: brak journalctl, zakładam że tablet zawsze czuwa")
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
		log.Printf("sleep-watch: journalctl zakończył się, wznawiam")
		time.Sleep(5 * time.Second)
	}
}

// syncInitialState ustala stan na podstawie ostatniego wpisu od startu systemu.
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

// isStreamPath mówi, które ścieżki upstreamu niosą obraz lub zdarzenia z ekranu.
func isStreamPath(p string) bool {
	switch p {
	case "/stream", "/screenshot", "/events", "/gestures", "/raw":
		return true
	}
	return false
}

// gateSleep blokuje ścieżki strumieniowe, gdy tablet śpi, a aktywne żądania
// zrywa w chwili zaśnięcia.
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

// handleState zwraca stan tabletu. Z parametrem ?wait=<version> działa jako
// long-poll: jeśli wersja się nie zmieniła, czeka do 25 s na zmianę (orientacji
// lub uśpienia), więc przeglądarka dowiaduje się o obrocie od razu.
func handleState(sw *sleepWatcher, vs *viewerSignal) http.HandlerFunc {
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
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
