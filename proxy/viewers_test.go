package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLight records every brightness value written.
type fakeLight struct {
	dir string
	mu  sync.Mutex
}

func newFakeLight(t *testing.T, cur int) *fakeLight {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "brightness"), []byte(itoa(cur)), 0o644)
	os.WriteFile(filepath.Join(d, "max_brightness"), []byte("2047"), 0o644)
	return &fakeLight{dir: d}
}

func itoa(i int) string { return fmtInt(i) }
func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func (f *fakeLight) value(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "brightness"))
	return strings.TrimSpace(string(b))
}

func TestPulseRestoresBrightness(t *testing.T) {
	for _, cur := range []int{0, 300, 1734, 2047} {
		fl := newFakeLight(t, cur)
		v := newViewerSignal(fl.dir, func() bool { return true })
		v.pulse(2, 10*time.Millisecond)
		if got := fl.value(t); got != fmtInt(cur) {
			t.Errorf("cur=%d: brightness after pulse = %s", cur, got)
		}
	}
}

func TestPulseSkippedWhileAsleep(t *testing.T) {
	fl := newFakeLight(t, 1000)
	before, _ := os.Stat(filepath.Join(fl.dir, "brightness"))
	v := newViewerSignal(fl.dir, func() bool { return false })
	v.pulse(2, 10*time.Millisecond)
	after, _ := os.Stat(filepath.Join(fl.dir, "brightness"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("brightness was written while the tablet was asleep")
	}
}

func TestViewerLifecycle(t *testing.T) {
	fl := newFakeLight(t, 1000)
	v := newViewerSignal(fl.dir, func() bool { return true })
	v.connectDelay = 50 * time.Millisecond
	v.goneDelay = 100 * time.Millisecond

	release := make(chan struct{})
	h := v.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))

	// A short-lived request (page refresh) must not announce.
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil))
	time.Sleep(10 * time.Millisecond)
	release <- struct{}{}
	time.Sleep(80 * time.Millisecond)
	v.mu.Lock()
	if v.announced {
		t.Error("announced a viewer that left before connectDelay")
	}
	v.mu.Unlock()

	// A real viewer announces after connectDelay.
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil)); close(done) }()
	time.Sleep(150 * time.Millisecond)
	v.mu.Lock()
	if !v.announced || v.active != 1 {
		t.Errorf("after connect: announced=%v active=%d", v.announced, v.active)
	}
	v.mu.Unlock()

	// Other paths are not counted.
	h2 := v.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/events", nil))
	if v.viewers() != 1 {
		t.Errorf("/events counted as a viewer: %d", v.viewers())
	}

	// Leaving resets the announcement after goneDelay, brightness restored.
	release <- struct{}{}
	<-done
	// connect pulse (~0.9 s) and gone pulse (~0.9 s) run one after another
	time.Sleep(2500 * time.Millisecond)
	v.mu.Lock()
	if v.announced || v.active != 0 {
		t.Errorf("after leave: announced=%v active=%d", v.announced, v.active)
	}
	v.mu.Unlock()
	if got := fl.value(t); got != "1000" {
		t.Errorf("brightness after signals = %s", got)
	}
}
