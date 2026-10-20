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

// Zdalne pióro: przeglądarka wysyła przez WebSocket /pen ruchy myszy/dotyku
// (współrzędne znormalizowane 0..1 w natywnym, pionowym układzie ekranu), a
// proxy wstrzykuje je jako zdarzenia evdev do urządzenia pióra. xochitl czyta
// to urządzenie bezpośrednio, więc traktuje je jak prawdziwe pióro, ze stałym
// naciskiem (penPressureFraction).
//
// Protokół (ramki tekstowe, kilka komend w ramce oddzielonych '\n'):
//
//	d <x> <y>   przyłożenie pióra
//	m <x> <y>   ruch (z naciskiem, jeśli przyłożone)
//	u           podniesienie
const penPressureFraction = 0.5

const (
	evSyn, evKey, evAbs                  = 0, 1, 3
	absX, absY, absPressure, absDistance = 0x00, 0x01, 0x18, 0x19
	btnToolPen, btnTouch                 = 0x140, 0x14a

	penStallTimeout = 1500 * time.Millisecond // przyłożone pióro bez ruchu -> podnieś
)

type penWriter struct {
	path string

	mu             sync.Mutex
	f              *os.File
	maxX, maxY     int32
	pressure       int32
	owner          any // sesja, która ma teraz pióro (nil = wolne)
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
	req := uintptr(0x80184540 + code) // EVIOCGABS(code)
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
	// Wartości domyślne z reMarkable Paper Pro, gdyby ioctl się nie udał.
	p.maxX = readAbsMax(f, absX, 11180)
	p.maxY = readAbsMax(f, absY, 15340)
	p.pressure = int32(float64(readAbsMax(f, absPressure, 4096)) * penPressureFraction)
	log.Printf("zdalne pióro: %s (X 0..%d, Y 0..%d, nacisk %d)", p.path, p.maxX, p.maxY, p.pressure)
	return nil
}

func (p *penWriter) emitLocked(typ, code uint16, value int32) {
	var b [24]byte // struct input_event na 64-bit: timeval(16) + type + code + value
	now := time.Now()
	binary.LittleEndian.PutUint64(b[0:], uint64(now.Unix()))
	binary.LittleEndian.PutUint64(b[8:], uint64(now.Nanosecond()/1000))
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))
	if _, err := p.f.Write(b[:]); err != nil {
		log.Printf("zdalne pióro: zapis: %v", err)
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

// apply wykonuje jedną komendę od sesji. Zwraca false, gdy pióro trzyma inna sesja.
func (p *penWriter) apply(owner any, cmd byte, x, y float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner != nil && p.owner != owner {
		return false
	}
	if err := p.openLocked(); err != nil {
		log.Printf("zdalne pióro: %v", err)
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
	case 'm':
		if !p.touch {
			return true // ruch bez przyłożenia ignorujemy (nie ma "najeżdżania")
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

// liftLocked podnosi i oddala pióro, zwalniając je dla innych sesji.
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

// release wywoływane, gdy sesja się kończy: nigdy nie zostawiaj przyłożonego pióra.
func (p *penWriter) release(owner any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner == owner {
		p.liftLocked()
	}
}

func (p *penWriter) watchdog() {
	for range time.Tick(500 * time.Millisecond) {
		p.mu.Lock()
		if p.touch && time.Since(p.lastEvent) > penStallTimeout {
			log.Printf("zdalne pióro: brak ruchu od %v, podnoszę", penStallTimeout)
			p.liftLocked()
		}
		p.mu.Unlock()
	}
}

// parsePenFrame dzieli ramkę na komendy.
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

// sameOrigin chroni WebSocket przed użyciem z obcej strony (cross-site).
func sameOrigin(cfg *websocket.Config, r *http.Request) error {
	o := r.Header.Get("Origin")
	if o == "" {
		return nil // nie-przeglądarkowy klient; i tak wymaga ciasteczka sesji
	}
	u, err := url.Parse(o)
	if err != nil || u.Host != r.Host {
		return fmt.Errorf("origin %q nie pasuje do hosta %q", o, r.Host)
	}
	return nil
}

func handlePen(pw *penWriter, sw *sleepWatcher) http.Handler {
	return websocket.Server{
		Handshake: sameOrigin,
		Handler: func(ws *websocket.Conn) {
			session := new(int) // unikalny identyfikator sesji
			defer pw.release(session)
			log.Printf("zdalne pióro: sesja otwarta (%s)", clientIP(ws.Request()))
			defer log.Printf("zdalne pióro: sesja zamknięta")
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
