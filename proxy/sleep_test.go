package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseDisplayState(t *testing.T) {
	cases := map[string]string{
		"21:47:53.317 rm.batterymanager        Changing display state from Normal to DeepSleep (setDisplayState /home/x)": "DeepSleep",
		"21:48:42.692 rm.batterymanager        Changing display state from DeepSleep to Normal (setDisplayState)":         "Normal",
		"21:48:42.673 rm.batterymanager        Waking up display from keyEvent=16777505":                                  "",
	}
	for line, want := range cases {
		got, ok := parseDisplayState(line)
		if (want == "") != !ok || got != want {
			t.Errorf("%q: got (%q,%v), want %q", line, got, ok, want)
		}
	}
}

func TestParseOrientation(t *testing.T) {
	cases := map[string]string{
		"08:23:25.352 \x1b[01;32mrm.orientationmodule     Setting new orientation Landscape\x1b[0;37m (setOrientation /x/orientationwrapper.cpp:82)":  "Landscape",
		"08:23:33.016 rm.orientationmodule     Setting new orientation InvertedLandscape (setOrientation /x/orientationwrapper.cpp:82":                "InvertedLandscape",
		"09:07:24.788 \x1b[01;32mrm.orientationmodule     Setting forced portrait window orientation Portrait\x1b[0;37m (operator() /x/setup.cpp:63)": "Portrait",
		"21:48:42.673 rm.batterymanager        Waking up display from keyEvent=16777505":                                                              "",
		"rm.orientationmodule     Setting new orientation Sideways":                                                                                   "",
	}
	for line, want := range cases {
		got, ok := parseOrientation(line)
		if (want == "") != !ok || got != want {
			t.Errorf("%q: got (%q,%v), want %q", line, got, ok, want)
		}
	}
}

func TestStateLongPollWakesOnOrientation(t *testing.T) {
	sw := newSleepWatcher()
	h := handleState(sw, nil, false)
	done := make(chan string, 1)
	go func() {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/state?wait=1", nil))
		done <- rec.Body.String()
	}()
	time.Sleep(100 * time.Millisecond)
	sw.handleLine("rm.orientationmodule     Setting new orientation Landscape (setOrientation x)")
	select {
	case body := <-done:
		if !strings.Contains(body, `"orientation":"Landscape"`) || !strings.Contains(body, `"version":2`) {
			t.Errorf("unexpected body %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not wake up on orientation change")
	}
}
