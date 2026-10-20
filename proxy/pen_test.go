package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

type ev struct {
	typ, code uint16
	value     int32
}

func readEvents(t *testing.T, path string) []ev {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []ev
	for i := 0; i+24 <= len(b); i += 24 {
		out = append(out, ev{binary.LittleEndian.Uint16(b[i+16:]), binary.LittleEndian.Uint16(b[i+18:]), int32(binary.LittleEndian.Uint32(b[i+20:]))})
	}
	return out
}

func newTestPen(t *testing.T) (*penWriter, string) {
	path := filepath.Join(t.TempDir(), "event2")
	os.WriteFile(path, nil, 0o644)
	return &penWriter{path: path}, path
}

func TestPenStrokeSequence(t *testing.T) {
	pw, path := newTestPen(t)
	a := new(int)
	pw.apply(a, 'd', 0.5, 0.25)
	pw.apply(a, 'm', 1.2, -1) // clamped
	pw.apply(a, 'u', 0, 0)
	evs := readEvents(t, path)

	has := func(want ev) bool {
		for _, e := range evs {
			if e == want {
				return true
			}
		}
		return false
	}
	for _, want := range []ev{
		{evKey, btnToolPen, 1}, {evAbs, absX, 5590}, {evAbs, absY, 3835},
		{evAbs, absPressure, 2048}, {evKey, btnTouch, 1},
		{evAbs, absX, 11180}, {evAbs, absY, 0},
		{evKey, btnTouch, 0}, {evAbs, absPressure, 0}, {evKey, btnToolPen, 0},
	} {
		if !has(want) {
			t.Errorf("missing event %+v in %+v", want, evs)
		}
	}
	if last := evs[len(evs)-1]; last != (ev{evSyn, 0, 0}) {
		t.Errorf("stroke must end with SYN, got %+v", last)
	}
}

func TestPenSingleOwnerAndRelease(t *testing.T) {
	pw, path := newTestPen(t)
	a, b := new(int), new(int)
	pw.apply(a, 'd', 0.1, 0.1)
	if pw.apply(b, 'd', 0.9, 0.9) {
		t.Error("second session took the pen while the first was drawing")
	}
	pw.release(a) // connection dropped mid-stroke
	evs := readEvents(t, path)
	if last := evs[len(evs)-2]; last != (ev{evKey, btnToolPen, 0}) {
		t.Errorf("release did not take the pen out of range: %+v", evs[len(evs)-4:])
	}
	if !pw.apply(b, 'd', 0.9, 0.9) {
		t.Error("pen not free after release")
	}
}

func TestParsePenFrame(t *testing.T) {
	var got []string
	parsePenFrame("d 0.1 0.2\nm 0.3 0.4\nx junk\nm bad\nu\n", func(c byte, x, y float64) {
		got = append(got, string(c))
	})
	if want := "dmu"; len(got) != 3 || got[0]+got[1]+got[2] != want {
		t.Errorf("got %v", got)
	}
}
