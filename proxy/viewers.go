package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// viewerSignal sygnalizuje na tablecie, że ktoś zaczął lub przestał oglądać
// strumień: mrugnięciem podświetlenia (front light). xochitl nie ma lokalnego
// API powiadomień, a e-ink nie odświeży pikseli dopisanych z zewnątrz, więc to
// najpewniejszy sygnał widoczny dla osoby z tabletem w ręku.
//
//   - widz połączony (strumień trwa >= connectDelay): dwa krótkie mrugnięcia,
//   - ostatni widz odszedł (brak widzów przez goneDelay): jedno długie.
//
// Opóźnienia odfiltrowują odświeżenia strony i chwilowe zerwania połączenia.
type viewerSignal struct {
	light        string // katalog sysfs podświetlenia, "" = wyłączone
	isAwake      func() bool
	connectDelay time.Duration
	goneDelay    time.Duration

	mu        sync.Mutex
	active    int
	announced bool        // zasygnalizowano "połączony", jeszcze nie "odszedł"
	goneTimer *time.Timer // odlicza goneDelay po odejściu ostatniego widza

	pulseMu sync.Mutex // serializuje mrugnięcia, żeby zawsze wrócić do jasności użytkownika
}

func newViewerSignal(light string, isAwake func() bool) *viewerSignal {
	return &viewerSignal{
		light: light, isAwake: isAwake,
		connectDelay: 2 * time.Second, goneDelay: 15 * time.Second,
	}
}

func (v *viewerSignal) viewers() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.active
}

// wrap liczy aktywne strumienie /stream przechodzące przez proxy.
func (v *viewerSignal) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream" {
			next.ServeHTTP(w, r)
			return
		}
		v.connected()
		defer v.disconnected()
		next.ServeHTTP(w, r)
	})
}

func (v *viewerSignal) connected() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.active++
	if v.goneTimer != nil {
		v.goneTimer.Stop()
		v.goneTimer = nil
	}
	if v.announced {
		return
	}
	time.AfterFunc(v.connectDelay, func() {
		v.mu.Lock()
		fire := v.active > 0 && !v.announced
		if fire {
			v.announced = true
		}
		n := v.active
		v.mu.Unlock()
		if fire {
			log.Printf("widz połączony (aktywne strumienie: %d) - sygnalizuję na tablecie", n)
			v.pulse(2, 300*time.Millisecond)
		}
	})
}

func (v *viewerSignal) disconnected() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.active--
	if v.active > 0 || !v.announced {
		return
	}
	if v.goneTimer != nil {
		v.goneTimer.Stop()
	}
	v.goneTimer = time.AfterFunc(v.goneDelay, func() {
		v.mu.Lock()
		fire := v.active == 0 && v.announced
		if fire {
			v.announced = false
		}
		v.mu.Unlock()
		if fire {
			log.Printf("ostatni widz odszedł - sygnalizuję na tablecie")
			v.pulse(1, 900*time.Millisecond)
		}
	})
}

// pulse mruga podświetleniem n razy i przywraca dokładnie poprzednią jasność.
// Pomija sygnał, gdy tablet śpi (nie chcemy zapalać światła uśpionemu tabletowi).
func (v *viewerSignal) pulse(n int, on time.Duration) {
	if v.light == "" || (v.isAwake != nil && !v.isAwake()) {
		return
	}
	v.pulseMu.Lock()
	defer v.pulseMu.Unlock()

	path := filepath.Join(v.light, "brightness")
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("sygnał: nie mogę odczytać podświetlenia: %v", err)
		return
	}
	cur, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return
	}
	max := 2047
	if mb, err := os.ReadFile(filepath.Join(v.light, "max_brightness")); err == nil {
		if m, err := strconv.Atoi(strings.TrimSpace(string(mb))); err == nil && m > 0 {
			max = m
		}
	}
	// Jasne światło -> mrugnięcie przygaszeniem; ciemne/wyłączone -> rozjaśnieniem.
	level := cur / 8
	if cur <= max/2 {
		level = cur + max*45/100
		if level > max {
			level = max
		}
	}
	set := func(x int) { _ = os.WriteFile(path, []byte(strconv.Itoa(x)), 0o644) }
	defer set(cur)
	for i := 0; i < n; i++ {
		set(level)
		time.Sleep(on)
		set(cur)
		if i < n-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}
}
