package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/websocket"
)

// Remote pen: the browser sends mouse/touch movements over the /pen WebSocket
// (coordinates normalised to 0..1 in the native portrait screen space), and
// the proxy injects them as evdev events into the pen input device. xochitl
// reads that device directly, so it treats them like the real pen, with a
// fixed pressure (penPressureFraction).
//
// Protocol (text frames, several commands per frame separated by '\n'):
//
//	d <x> <y>   pen down
//	m <x> <y>   move (with pressure while the pen is down)
//	u           pen up
const penPressureFraction = 0.5

const (
	evSyn, evKey, evAbs                  = 0, 1, 3
	absX, absY, absPressure, absDistance = 0x00, 0x01, 0x18, 0x19
	btnToolPen, btnTouch                 = 0x140, 0x14a

	penStallTimeout = 1500 * time.Millisecond // pen down without movement -> lift it
)

type penWriter struct {
	path string

	mu             sync.Mutex
	f              *os.File
	maxX, maxY     int32
	pressure       int32
	owner          any // session currently holding the pen (nil = free)
	reserved       any // drawing job keeping the pen between its strokes (nil = none)
	inRange, touch bool
	lastEvent      time.Time
}

func newPenWriter(path string) *penWriter {
	p := &penWriter{path: path}
	go p.watchdog()
	return p
}

type absInfo struct{ Value, Min, Max, Fuzz, Flat, Res int32 }

func readAbsMax(f *os.File, code int, fallback int32) int32 {
	var a absInfo
	req := uintptr(0x80184540) + uintptr(code) // EVIOCGABS(code)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&a))); e != 0 || a.Max <= 0 {
		return fallback
	}
	return a.Max
}

func (p *penWriter) openLocked() error {
	if p.f != nil {
		return nil
	}
	f, err := os.OpenFile(p.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	p.f = f
	// reMarkable Paper Pro values, used if the ioctl fails.
	p.maxX = readAbsMax(f, absX, 11180)
	p.maxY = readAbsMax(f, absY, 15340)
	p.pressure = int32(float64(readAbsMax(f, absPressure, 4096)) * penPressureFraction)
	log.Printf("remote pen: %s (X 0..%d, Y 0..%d, pressure %d)", p.path, p.maxX, p.maxY, p.pressure)
	return nil
}

// wordSize is the size of a C long. struct timeval is two longs, so
// struct input_event is 24 bytes on arm64 and 16 bytes on 32-bit ARM.
const wordSize = int(unsafe.Sizeof(uintptr(0)))

// encodeInputEvent serialises a struct input_event for the running architecture.
func encodeInputEvent(now time.Time, typ, code uint16, value int32) []byte {
	b := make([]byte, 2*wordSize+8)
	if wordSize == 8 {
		binary.LittleEndian.PutUint64(b[0:], uint64(now.Unix()))
		binary.LittleEndian.PutUint64(b[8:], uint64(now.Nanosecond()/1000))
	} else {
		binary.LittleEndian.PutUint32(b[0:], uint32(now.Unix()))
		binary.LittleEndian.PutUint32(b[4:], uint32(now.Nanosecond()/1000))
	}
	t := 2 * wordSize
	binary.LittleEndian.PutUint16(b[t:], typ)
	binary.LittleEndian.PutUint16(b[t+2:], code)
	binary.LittleEndian.PutUint32(b[t+4:], uint32(value))
	return b
}

func (p *penWriter) emitLocked(typ, code uint16, value int32) {
	if _, err := p.f.Write(encodeInputEvent(time.Now(), typ, code, value)); err != nil {
		log.Printf("remote pen: write: %v", err)
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// apply executes one command from a session. It returns false when another
// session holds the pen.
func (p *penWriter) apply(owner any, cmd byte, x, y float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if (p.owner != nil && p.owner != owner) || (p.reserved != nil && p.reserved != owner) {
		return false
	}
	if err := p.openLocked(); err != nil {
		log.Printf("remote pen: %v", err)
		return false
	}
	p.lastEvent = time.Now()
	ax, ay := int32(clamp01(x)*float64(p.maxX)), int32(clamp01(y)*float64(p.maxY))
	switch cmd {
	case 'd':
		p.owner = owner
		if !p.inRange {
			p.emitLocked(evKey, btnToolPen, 1)
			p.emitLocked(evAbs, absX, ax)
			p.emitLocked(evAbs, absY, ay)
			p.emitLocked(evAbs, absDistance, 10)
			p.emitLocked(evSyn, 0, 0)
			p.inRange = true
		}
		p.emitLocked(evAbs, absX, ax)
		p.emitLocked(evAbs, absY, ay)
		p.emitLocked(evAbs, absDistance, 0)
		p.emitLocked(evAbs, absPressure, p.pressure)
		p.emitLocked(evKey, btnTouch, 1)
		p.emitLocked(evSyn, 0, 0)
		p.touch = true
	case 'h':
		// Hover: in range above the screen, not touching. Drawing jobs move
		// the pen like this before touching down, as a hand does; xochitl
		// smooths strokes with some inertia and needs the approach.
		if p.touch {
			return true
		}
		p.owner = owner
		if !p.inRange {
			p.emitLocked(evKey, btnToolPen, 1)
			p.inRange = true
		}
		p.emitLocked(evAbs, absX, ax)
		p.emitLocked(evAbs, absY, ay)
		p.emitLocked(evAbs, absDistance, 10)
		p.emitLocked(evSyn, 0, 0)
	case 'm':
		if !p.touch {
			return true // ignore moves while the pen is up (no hover)
		}
		p.emitLocked(evAbs, absX, ax)
		p.emitLocked(evAbs, absY, ay)
		p.emitLocked(evAbs, absPressure, p.pressure)
		p.emitLocked(evSyn, 0, 0)
	case 'u':
		p.liftLocked()
	}
	return true
}

// liftLocked lifts the pen and takes it out of range, freeing it for other
// sessions.
func (p *penWriter) liftLocked() {
	if p.f == nil {
		return
	}
	if p.touch {
		p.emitLocked(evAbs, absPressure, 0)
		p.emitLocked(evKey, btnTouch, 0)
		p.emitLocked(evAbs, absDistance, 10)
		p.emitLocked(evSyn, 0, 0)
		p.touch = false
	}
	if p.inRange {
		p.emitLocked(evKey, btnToolPen, 0)
		p.emitLocked(evSyn, 0, 0)
		p.inRange = false
	}
	p.owner = nil
}

// release is called when a session ends: never leave the pen pressed down.
func (p *penWriter) release(owner any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner == owner {
		p.liftLocked()
	}
	if p.reserved == owner {
		p.reserved = nil
	}
}

// reserve keeps the pen for owner until release(owner), so that nobody else
// can draw between the strokes of a drawing job. It fails while someone
// else holds or reserved the pen.
func (p *penWriter) reserve(owner any) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if (p.owner != nil && p.owner != owner) || (p.reserved != nil && p.reserved != owner) {
		return false
	}
	p.reserved = owner
	return true
}

func (p *penWriter) watchdog() {
	for range time.Tick(500 * time.Millisecond) {
		p.mu.Lock()
		if p.touch && time.Since(p.lastEvent) > penStallTimeout {
			log.Printf("remote pen: no movement for %v, lifting", penStallTimeout)
			p.liftLocked()
		}
		p.mu.Unlock()
	}
}

// parsePenFrame splits a frame into commands.
func parsePenFrame(frame string, fn func(cmd byte, x, y float64)) {
	sc := bufio.NewScanner(strings.NewReader(frame))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var x, y float64
		switch line[0] {
		case 'd', 'm':
			if _, err := fmt.Sscan(line[1:], &x, &y); err != nil {
				continue
			}
		case 'u':
		default:
			continue
		}
		fn(line[0], x, y)
	}
}

// sameOrigin protects the WebSocket from being used by another site
// (cross-site WebSocket hijacking).
func sameOrigin(cfg *websocket.Config, r *http.Request) error {
	o := r.Header.Get("Origin")
	if o == "" {
		return nil // non-browser client; still needs the session cookie
	}
	u, err := url.Parse(o)
	if err != nil || u.Host != r.Host {
		return fmt.Errorf("origin %q does not match host %q", o, r.Host)
	}
	return nil
}

func handlePen(pw *penWriter, sw *sleepWatcher) http.Handler {
	return websocket.Server{
		Handshake: sameOrigin,
		Handler: func(ws *websocket.Conn) {
			session := new(int) // unique session identity
			defer pw.release(session)
			log.Printf("remote pen: session opened (%s)", clientIP(ws.Request()))
			defer log.Printf("remote pen: session closed")
			for {
				var frame string
				if err := websocket.Message.Receive(ws, &frame); err != nil {
					return
				}
				if sw != nil && !sw.isAwake() {
					pw.release(session)
					continue
				}
				parsePenFrame(frame, func(cmd byte, x, y float64) {
					pw.apply(session, cmd, x, y)
				})
			}
		},
	}
}
