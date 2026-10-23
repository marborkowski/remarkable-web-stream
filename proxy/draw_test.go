package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

// testFont builds a minimal sfnt with a name table holding fullName and
// three dummy tables.
func testFont(fullName string) []byte {
	name16 := utf16.Encode([]rune(fullName))
	var name bytes.Buffer
	be := func(v ...uint16) {
		for _, x := range v {
			binary.Write(&name, binary.BigEndian, x)
		}
	}
	be(0, 1, 6+12)                               // format, count, string offset
	be(3, 1, 0x409, 4, uint16(2*len(name16)), 0) // Windows, Unicode BMP, en-US, full name
	binary.Write(&name, binary.BigEndian, name16)
	tables := []struct {
		tag  string
		data []byte
	}{{"cmap", []byte{1, 2, 3, 4}}, {"glyf", []byte{5, 6, 7, 8}}, {"head", []byte{9, 9, 9, 9}}, {"name", name.Bytes()}}
	var f bytes.Buffer
	binary.Write(&f, binary.BigEndian, uint32(0x00010000))
	binary.Write(&f, binary.BigEndian, []uint16{uint16(len(tables)), 0, 0, 0})
	off := 12 + 16*len(tables)
	for _, t := range tables {
		f.WriteString(t.tag)
		binary.Write(&f, binary.BigEndian, []uint32{0, uint32(off), uint32(len(t.data))})
		off += len(t.data)
	}
	for _, t := range tables {
		f.Write(t.data)
	}
	return f.Bytes()
}

func TestFindFont(t *testing.T) {
	sans := testFont(textFontName)
	serif := testFont("reMarkable Serif Small Regular")
	junk := []byte("\x7fELF\x00\x01\x00\x00garbage\x00\x01\x00\x00\x00\x04 not a font")
	bin := bytes.Join([][]byte{junk, serif, junk, sans, junk}, nil)

	got := findFont(bin, textFontName)
	if !bytes.Equal(got, sans) {
		t.Fatalf("findFont: got %d bytes, want the %d-byte font", len(got), len(sans))
	}
	if sfntFullName(got) != textFontName {
		t.Errorf("name %q", sfntFullName(got))
	}
	if findFont(bin, "Comic Sans") != nil {
		t.Error("found a font that is not there")
	}
	if findFont(sans[:len(sans)-3], textFontName) != nil {
		t.Error("accepted a truncated font")
	}
}

func TestDrawJobValidate(t *testing.T) {
	minusOne, three := -1, 3
	ok := drawJob{Text: "\n\u2028 A -> B", PagesUp: &three, Strokes: [][]float64{{0.1, 0.1, 0.2, 0.2}}}
	if err := ok.validate(); err != nil {
		t.Errorf("valid job: %v", err)
	}
	for name, j := range map[string]drawJob{
		"odd coordinates":  {Strokes: [][]float64{{0.1, 0.2, 0.3}}},
		"empty stroke":     {Strokes: [][]float64{{}}},
		"untypeable":       {Text: "[x]"},
		"too long":         {Text: strings.Repeat("a", maxDrawText+1)},
		"negative pagesUp": {PagesUp: &minusOne},
	} {
		if err := j.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDrawStrokes(t *testing.T) {
	pw, path := newTestPen(t)
	d := newDrawer(pw, nil, nil, "")
	job := &drawJob{Strokes: [][]float64{{0, 0, 0.5, 0.5}, {1, 1}}}
	var last int
	err := d.execute(context.Background(), job, func(string) func(int, int) {
		return func(done, total int) { last = done }
	})
	if err != nil {
		t.Fatal(err)
	}
	if last != 3 {
		t.Errorf("progress ended at %d, want 3", last)
	}
	touches := 0
	for _, e := range readEvents(t, path) {
		if e.typ == evKey && e.code == btnTouch && e.value == 1 {
			touches++
		}
	}
	if touches != 2 {
		t.Errorf("%d pen-down events, want 2", touches)
	}
	// The job released the pen.
	if !pw.apply(new(int), 'd', 0.5, 0.5) {
		t.Error("pen still reserved after the job")
	}
}

func TestPenReservation(t *testing.T) {
	pw, _ := newTestPen(t)
	job, other := new(int), new(int)
	if !pw.reserve(job) {
		t.Fatal("reserve failed")
	}
	pw.apply(job, 'd', 0.1, 0.1)
	pw.apply(job, 'u', 0, 0)
	if pw.apply(other, 'd', 0.1, 0.1) {
		t.Error("another session drew between the job's strokes")
	}
	if pw.reserve(other) {
		t.Error("reserved twice")
	}
	pw.release(job)
	if !pw.apply(other, 'd', 0.1, 0.1) {
		t.Error("pen not free after release")
	}
}

func TestLeadIn(t *testing.T) {
	// A stroke going right from the middle: the pen hovers in from the left.
	s := []float64{0.5, 0.5, 0.6, 0.5}
	lead := leadIn(s)
	if len(lead) != leadInSamples {
		t.Fatalf("%d hover samples, want %d", len(lead), leadInSamples)
	}
	first, last := lead[0], lead[len(lead)-1]
	if !(first[0] < last[0] && last[0] < 0.5) || first[1] != 0.5 || last[1] != 0.5 {
		t.Errorf("lead-in %v .. %v does not approach (0.5, 0.5) from the left", first, last)
	}
	if d := 0.5 - first[0]; d < 0.03 || d > 0.04 {
		t.Errorf("lead-in starts %.4f away, want about %.4f", d, leadInSamples*leadInStep)
	}
	// Near the edge it stays on the screen; a dot is approached from above.
	for _, p := range append(leadIn([]float64{0.001, 0.2, 0.1, 0.2}), leadIn([]float64{0.3, 0.3})...) {
		if p[0] < 0 || p[0] > 1 || p[1] < 0 || p[1] > 1 {
			t.Errorf("lead-in point %v off the screen", p)
		}
	}
	if dot := leadIn([]float64{0.3, 0.3}); dot[0][0] != 0.3 || dot[0][1] >= 0.3 {
		t.Errorf("dot lead-in starts at %v, want above (0.3, 0.3)", dot[0])
	}
}

func TestParseCPUTicks(t *testing.T) {
	stat := "350 (xochitl (main)) S 1 350 350 0 -1 4194560 81024 0 0 0 1234 567 0 0 20 0 30 0 1500 900000000 50000"
	if n, ok := parseCPUTicks(stat); !ok || n != 1234+567 {
		t.Errorf("got %d, %v; want %d", n, ok, 1234+567)
	}
	if _, ok := parseCPUTicks("garbage"); ok {
		t.Error("parsed garbage")
	}
}
