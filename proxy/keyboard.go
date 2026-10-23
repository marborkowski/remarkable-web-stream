package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

// Virtual keyboard: types text into xochitl's own text tool.
//
// xochitl discovers keyboards through udev (that is how the Type Folio
// works), so the proxy creates a keyboard with uinput for the duration of a
// typing job and removes it afterwards. When a keyboard appears on an empty
// page, xochitl starts the page's text in the Title style; on a page that
// already has text, the first key press puts it back in text mode. The text
// of a page is one flowing document: Enter starts a paragraph, Shift+Enter a
// line within it.
//
// Keys are mapped for the "US" Type Folio layout, which xochitl uses when no
// Type Folio is attached. It has no [ ] { } keys, and ´ ` ^ ¨ ~ are dead keys:
// followed by a letter they compose an accented letter, followed by a space
// they produce the accent itself.
const (
	keyboardSettle = 1500 * time.Millisecond // xochitl needs this long to pick up a new keyboard
	textModeSettle = 500 * time.Millisecond  // the first key switches xochitl to text mode; keys meanwhile are lost
	keyboardLinger = 500 * time.Millisecond  // let xochitl process the last keys before unplugging
	keyHold        = 4 * time.Millisecond
	keyGap         = 8 * time.Millisecond
	// Around a dead key and the letter it composes. Without it xochitl,
	// which lags behind fast typing, sometimes puts the composed letter
	// before the letters typed just ahead of it ("Zaómwienie").
	deadKeyPause = 60 * time.Millisecond
	typeBatch    = 5 // keys
	batchPoll    = 30 * time.Millisecond
	batchWait    = 3 * time.Second
)

const (
	key4         = 5
	keyEnter     = 28
	keyLeftCtrl  = 29
	keyA         = 30
	keyLeftShift = 42
	keySpace     = 57
	keyPageUp    = 104
	keyRight     = 106
)

// keyStroke is one key press, possibly with Shift held.
type keyStroke struct {
	code  uint16
	shift bool
}

// isDead reports whether k is one of the dead keys below.
func (k keyStroke) isDead() bool {
	return k.code == 26 || k.code == 27 || (k.code == 7 && k.shift)
}

var (
	deadAcute      = keyStroke{26, false}
	deadGrave      = keyStroke{26, true}
	deadDiaeresis  = keyStroke{27, false}
	deadTilde      = keyStroke{27, true}
	deadCircumflex = keyStroke{7, true}
)

// usKeys maps the characters typed with a single key press.
var usKeys = map[rune]keyStroke{}

// usComposed maps the characters typed with a dead key and a letter (or a
// space).
var usComposed = map[rune][2]keyStroke{}

func init() {
	rows := []struct {
		first            uint16
		plain, shifted   string
		skipShiftedIndex int // -1 = none
	}{
		{2, "1234567890-=", "!@#$%^&*()_+", 5}, // Shift+6 is the dead circumflex
		{16, "qwertyuiop", "QWERTYUIOP", -1},
		{30, "asdfghjkl;'", "ASDFGHJKL:\"", -1},
		{44, "zxcvbnm,./", "ZXCVBNM<>?", -1},
	}
	for _, row := range rows {
		shifted := []rune(row.shifted)
		for i, r := range row.plain {
			code := row.first + uint16(i)
			usKeys[r] = keyStroke{code, false}
			if i != row.skipShiftedIndex {
				usKeys[shifted[i]] = keyStroke{code, true}
			}
		}
	}
	usKeys['\\'] = keyStroke{43, false}
	usKeys['|'] = keyStroke{43, true}
	usKeys[' '] = keyStroke{keySpace, false}
	usKeys['\n'] = keyStroke{keyEnter, false}    // new paragraph
	usKeys['\u2028'] = keyStroke{keyEnter, true} // LINE SEPARATOR: new line in the paragraph

	space := keyStroke{keySpace, false}
	usComposed['´'] = [2]keyStroke{deadAcute, space}
	usComposed['`'] = [2]keyStroke{deadGrave, space}
	usComposed['¨'] = [2]keyStroke{deadDiaeresis, space}
	usComposed['~'] = [2]keyStroke{deadTilde, space}
	usComposed['^'] = [2]keyStroke{deadCircumflex, space}
	for _, c := range []struct {
		dead          keyStroke
		bases, result string
	}{
		{deadAcute, "aeiouycnsz", "áéíóúýćńśź"},
		{deadGrave, "aeiou", "àèìòù"},
		{deadCircumflex, "aeiou", "âêîôû"},
		{deadDiaeresis, "aeiou", "äëïöü"},
		{deadTilde, "ano", "ãñõ"},
	} {
		results := []rune(c.result)
		for i, base := range c.bases {
			usComposed[results[i]] = [2]keyStroke{c.dead, usKeys[base]}
			usComposed[unicode.ToUpper(results[i])] = [2]keyStroke{c.dead, usKeys[unicode.ToUpper(base)]}
		}
	}
}

// keystrokes converts text to key presses. It fails on characters the
// layout cannot type; the browser replaces those before sending.
func keystrokes(text string) ([]keyStroke, error) {
	var out []keyStroke
	var bad []string
	for _, r := range text {
		if k, ok := usKeys[r]; ok {
			out = append(out, k)
		} else if k, ok := usComposed[r]; ok {
			out = append(out, k[0], k[1])
		} else {
			bad = append(bad, fmt.Sprintf("%q", r))
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("cannot type %s", strings.Join(bad, ", "))
	}
	return out, nil
}

// typeableChars lists every printable character the virtual keyboard can
// type, for the browser to replace the others.
func typeableChars() string {
	var b strings.Builder
	for r := range usKeys {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	for r := range usComposed {
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// uinput
// ---------------------------------------------------------------------------

const (
	uiDevCreate  = 0x5501     // _IO('U', 1)
	uiDevDestroy = 0x5502     // _IO('U', 2)
	uiDevSetup   = 0x405c5503 // _IOW('U', 3, struct uinput_setup)
	uiSetEvBit   = 0x40045564 // _IOW('U', 100, int)
	uiSetKeyBit  = 0x40045565 // _IOW('U', 101, int)
	busUSB       = 0x03
	maxKeyCode   = 248
)

type virtualKeyboard struct {
	path string // /dev/uinput
	mu   sync.Mutex
}

func newVirtualKeyboard(path string) *virtualKeyboard {
	return &virtualKeyboard{path: path}
}

type uinputDevice struct{ f *os.File }

func ioctl(f *os.File, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, arg); e != 0 {
		return e
	}
	return nil
}

func createKeyboard(path string) (*uinputDevice, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fail := func(what string, err error) (*uinputDevice, error) {
		f.Close()
		return nil, fmt.Errorf("uinput %s: %w", what, err)
	}
	if err := ioctl(f, uiSetEvBit, evKey); err != nil {
		return fail("UI_SET_EVBIT", err)
	}
	// All keyboard keys: udev only tags devices with the first 31 keys as
	// keyboards (ID_INPUT_KEYBOARD), and xochitl only opens those.
	for k := uintptr(1); k <= maxKeyCode; k++ {
		if err := ioctl(f, uiSetKeyBit, k); err != nil {
			return fail("UI_SET_KEYBIT", err)
		}
	}
	var setup [92]byte // struct uinput_setup: input_id, name[80], ff_effects_max
	binary.LittleEndian.PutUint16(setup[0:], busUSB)
	binary.LittleEndian.PutUint16(setup[2:], 0x1209) // pid.codes vendor ID for open-source projects
	binary.LittleEndian.PutUint16(setup[4:], 0x0001)
	binary.LittleEndian.PutUint16(setup[6:], 1) // version
	copy(setup[8:88], "rmshare virtual keyboard")
	if err := ioctl(f, uiDevSetup, uintptr(unsafe.Pointer(&setup[0]))); err != nil {
		return fail("UI_DEV_SETUP", err)
	}
	if err := ioctl(f, uiDevCreate, 0); err != nil {
		return fail("UI_DEV_CREATE", err)
	}
	return &uinputDevice{f}, nil
}

func (d *uinputDevice) destroy() {
	ioctl(d.f, uiDevDestroy, 0)
	d.f.Close()
}

func (d *uinputDevice) key(code uint16, down bool) {
	var v int32
	if down {
		v = 1
	}
	now := time.Now()
	ev := append(encodeInputEvent(now, evKey, code, v), encodeInputEvent(now, evSyn, 0, 0)...)
	if _, err := d.f.Write(ev); err != nil {
		log.Printf("virtual keyboard: write: %v", err)
	}
}

func (d *uinputDevice) press(k keyStroke, modifiers ...uint16) {
	if k.shift {
		modifiers = append(modifiers, keyLeftShift)
	}
	for _, m := range modifiers {
		d.key(m, true)
	}
	d.key(k.code, true)
	time.Sleep(keyHold)
	d.key(k.code, false)
	for _, m := range modifiers {
		d.key(m, false)
	}
	time.Sleep(keyGap)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// session plugs in the virtual keyboard, waits for xochitl to pick it up,
// puts the cursor at the end of the page's text, runs fn and unplugs the
// keyboard.
//
// The first key puts xochitl in text mode, and the keys right after it are
// lost, so it is a harmless Right followed by a pause. xochitl has no key for
// the end of the text (Ctrl+End does nothing), so the session selects all
// (Ctrl+A) and collapses the selection to its end (Right). That scrolls the
// page only if the cursor moves: after typing, which leaves the cursor at the
// end, the page stays where it is.
func (k *virtualKeyboard) session(ctx context.Context, fn func(dev *uinputDevice) error) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	dev, err := createKeyboard(k.path)
	if err != nil {
		return err
	}
	defer dev.destroy()
	if err := sleepCtx(ctx, keyboardSettle); err != nil {
		return err
	}
	dev.press(keyStroke{keyRight, false})
	if err := sleepCtx(ctx, textModeSettle); err != nil {
		return err
	}
	dev.press(keyStroke{keyA, false}, keyLeftCtrl)
	dev.press(keyStroke{keyRight, false})
	if err := fn(dev); err != nil {
		return err
	}
	return sleepCtx(ctx, keyboardLinger)
}

// typeText types text as a new paragraph after the page's text. progress is
// called after every key press.
//
// The keys go in small batches, each once xochitl has handled the one
// before: it lays text out more slowly than keys arrive, the more slowly
// the longer the text, and the kernel keeps only 64 input events (about 16
// keys) for it, dropping the rest.
func (k *virtualKeyboard) typeText(ctx context.Context, text string, progress func(done, total int)) error {
	strokes, err := keystrokes(text)
	if err != nil {
		return err
	}
	log.Printf("virtual keyboard: typing %d keys", len(strokes))
	xochitl := findProcess("xochitl")
	return k.session(ctx, func(dev *uinputDevice) error {
		// Enter, then Ctrl+4 (Body text): a new paragraph keeps the style
		// of the one before it, e.g. the Title an empty page starts with.
		dev.press(keyStroke{keyEnter, false})
		dev.press(keyStroke{key4, false}, keyLeftCtrl)
		for i, s := range strokes {
			if i%typeBatch == 0 && !(i > 0 && strokes[i-1].isDead()) {
				if err := caughtUp(ctx, xochitl); err != nil {
					return err
				}
			}
			composing := s.isDead() || (i > 0 && strokes[i-1].isDead())
			if s.isDead() {
				time.Sleep(deadKeyPause)
			}
			dev.press(s)
			if composing && !s.isDead() {
				time.Sleep(deadKeyPause)
			}
			progress(i+1, len(strokes))
		}
		return nil
	})
}

// scrollUp presses Page Up pages times, which in text mode scrolls the page
// up by 80 % of the screen each time (outside it, by more). This is how a
// drawing job reaches the parts of a diagram taller than the screen, from
// the bottom up: typing leaves the page at the end of the text.
func (k *virtualKeyboard) scrollUp(ctx context.Context, pages int) error {
	return k.session(ctx, func(dev *uinputDevice) error {
		for i := 0; i < pages; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			dev.press(keyStroke{keyPageUp, false})
		}
		return nil
	})
}

// caughtUp waits until xochitl has handled the keys sent so far, that is
// until it used (almost) no CPU time in two short polls in a row, at most
// batchWait. Without the process (tests) it only pauses.
func caughtUp(ctx context.Context, pid string) error {
	if pid == "" {
		return sleepCtx(ctx, batchPoll)
	}
	prev, ok := cpuTicks(pid)
	quiet := 0
	for start := time.Now(); ok && time.Since(start) < batchWait; {
		if err := sleepCtx(ctx, batchPoll); err != nil {
			return err
		}
		var cur int64
		if cur, ok = cpuTicks(pid); !ok {
			break
		}
		if cur-prev <= 1 {
			if quiet++; quiet == 2 {
				return nil
			}
		} else {
			quiet = 0
		}
		prev = cur
	}
	return nil
}
