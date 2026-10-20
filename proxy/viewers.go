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

// viewerSignal tells the person holding the tablet that someone started or
// stopped watching the stream, by blinking the front light. xochitl has no
// local notification API, and e-ink will not refresh pixels written from
// outside, so this is the most dependable signal visible on the device.
//
//   - viewer connected (stream open for >= connectDelay): two short blinks,
//   - last viewer left (no viewers for goneDelay): one long blink.
//
// The delays filter out page reloads and brief connection drops.
type viewerSignal struct {
	light        string // sysfs backlight directory, "" = disabled
	isAwake      func() bool
	connectDelay time.Duration
	goneDelay    time.Duration

	mu        sync.Mutex
	active    int
	announced bool        // "connected" was signalled, "left" not yet
	goneTimer *time.Timer // counts goneDelay down after the last viewer left

	pulseMu sync.Mutex // serialises blinks so the user brightness is always restored
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

// wrap counts active /stream requests passing through the proxy.
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
			log.Printf("viewer connected (active streams: %d) - signalling on the tablet", n)
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
			log.Printf("last viewer left - signalling on the tablet")
			v.pulse(1, 900*time.Millisecond)
		}
	})
}

// pulse blinks the front light n times and restores the exact previous
// brightness. It is skipped while the tablet sleeps (never light up a sleeping tablet).
func (v *viewerSignal) pulse(n int, on time.Duration) {
	if v.light == "" || (v.isAwake != nil && !v.isAwake()) {
		return
	}
	v.pulseMu.Lock()
	defer v.pulseMu.Unlock()

	path := filepath.Join(v.light, "brightness")
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("signal: cannot read the front light: %v", err)
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
	// Bright light -> blink by dimming; dim/off -> blink by brightening.
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
