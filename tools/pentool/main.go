// pentool: pen event diagnostics and injection test for the reMarkable.
//
//	pentool info   [dev]                 axis ranges (EVIOCGABS)
//	pentool record [dev] [seconds]       dump events from the real pen
//	pentool line   [dev] x1 y1 x2 y2     draw a line in screen pixels (native portrait)
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

const (
	evSyn = 0
	evKey = 1
	evAbs = 3

	absX, absY, absPressure, absDistance, absTiltX, absTiltY = 0x00, 0x01, 0x18, 0x19, 0x1a, 0x1b
	btnToolPen, btnTouch                                     = 0x140, 0x14a

	screenW, screenH = 1632, 2154
)

type absInfo struct{ Value, Min, Max, Fuzz, Flat, Res int32 }

func eviocgabs(abs int) uintptr { return uintptr(0x80184540) + uintptr(abs) } // _IOR('E', 0x40+abs, 24 bytes)

func getAbs(f *os.File, abs int) (absInfo, error) {
	var a absInfo
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), eviocgabs(abs), uintptr(unsafe.Pointer(&a)))
	if e != 0 {
		return a, e
	}
	return a, nil
}

// input_event on 64-bit: timeval (16 B) + type u16 + code u16 + value s32 = 24 B.
// pentool assumes this 64-bit layout (reMarkable Paper Pro); build it for arm64.
func write(f *os.File, typ, code uint16, value int32) {
	var b [24]byte
	now := time.Now()
	binary.LittleEndian.PutUint64(b[0:], uint64(now.Unix()))
	binary.LittleEndian.PutUint64(b[8:], uint64(now.Nanosecond()/1000))
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))
	if _, err := f.Write(b[:]); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
}

func syn(f *os.File) { write(f, evSyn, 0, 0) }

func main() {
	dev := "/dev/input/event2"
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("usage: pentool info|record|line ...")
		return
	}
	cmd := args[0]
	args = args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] == '/' {
		dev, args = args[0], args[1:]
	}
	switch cmd {
	case "info":
		f, err := os.Open(dev)
		if err != nil {
			panic(err)
		}
		for _, a := range []struct {
			n string
			c int
		}{{"X", absX}, {"Y", absY}, {"PRESSURE", absPressure}, {"DISTANCE", absDistance}, {"TILT_X", absTiltX}, {"TILT_Y", absTiltY}} {
			ai, err := getAbs(f, a.c)
			fmt.Printf("%-9s %+v err=%v\n", a.n, ai, err)
		}
	case "record":
		secs := 5
		if len(args) > 0 {
			secs, _ = strconv.Atoi(args[0])
		}
		f, err := os.Open(dev)
		if err != nil {
			panic(err)
		}
		deadline := time.Now().Add(time.Duration(secs) * time.Second)
		go func() { time.Sleep(time.Until(deadline)); f.Close() }()
		var b [24]byte
		for {
			if _, err := f.Read(b[:]); err != nil {
				return
			}
			fmt.Printf("%d.%06d type=%d code=%#x value=%d\n", binary.LittleEndian.Uint64(b[0:]), binary.LittleEndian.Uint64(b[8:]),
				binary.LittleEndian.Uint16(b[16:]), binary.LittleEndian.Uint16(b[18:]), int32(binary.LittleEndian.Uint32(b[20:])))
		}
	case "line":
		if len(args) < 4 {
			fmt.Println("line x1 y1 x2 y2")
			return
		}
		var c [4]float64
		for i := range c {
			c[i], _ = strconv.ParseFloat(args[i], 64)
		}
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			panic(err)
		}
		ax, _ := getAbs(f, absX)
		ay, _ := getAbs(f, absY)
		ap, _ := getAbs(f, absPressure)
		pressure := ap.Max * 6 / 10
		toAbs := func(px, py float64) (int32, int32) {
			return int32(px / screenW * float64(ax.Max)), int32(py / screenH * float64(ay.Max))
		}
		x, y := toAbs(c[0], c[1])
		// pen enters range
		write(f, evKey, btnToolPen, 1)
		write(f, evAbs, absX, x)
		write(f, evAbs, absY, y)
		write(f, evAbs, absDistance, 10)
		syn(f)
		time.Sleep(20 * time.Millisecond)
		// pen touches the screen
		write(f, evAbs, absDistance, 0)
		write(f, evAbs, absPressure, pressure)
		write(f, evKey, btnTouch, 1)
		syn(f)
		steps := 60
		for i := 1; i <= steps; i++ {
			t := float64(i) / float64(steps)
			x, y = toAbs(c[0]+(c[2]-c[0])*t, c[1]+(c[3]-c[1])*t)
			write(f, evAbs, absX, x)
			write(f, evAbs, absY, y)
			write(f, evAbs, absPressure, pressure)
			syn(f)
			time.Sleep(8 * time.Millisecond)
		}
		// lift and leave range
		write(f, evAbs, absPressure, 0)
		write(f, evKey, btnTouch, 0)
		write(f, evAbs, absDistance, 10)
		syn(f)
		time.Sleep(20 * time.Millisecond)
		write(f, evKey, btnToolPen, 0)
		syn(f)
		fmt.Printf("line injected (pressure %d/%d)\n", pressure, ap.Max)
	}
}
