package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	_ "embed"

	qrcode "github.com/skip2/go-qrcode"
)

// The login, join and invitation pages are html/template definitions in
// pages.html, sharing one stylesheet.

//go:embed pages.html
var pagesHTML string

var pages = template.Must(template.New("pages").Funcs(template.FuncMap{
	"when":  fmtTime,
	"until": fmtUntil,
	"ago":   fmtAgo,
}).Parse(pagesHTML))

// pageData is the template context; each page uses the fields it needs.
type pageData struct {
	Error string

	// join
	Token    string
	Invite   *invite
	SignedIn bool

	// invites
	Session   session
	Invites   []invite
	Sessions  []session
	Lifetimes []struct {
		Label string
		D     time.Duration
	}
	DefaultLifetime time.Duration
	NewInvite       *invite
	NewLink         string
	NewLinkQR       template.URL
}

func renderPage(w http.ResponseWriter, name string, status int, d pageData) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, d); err != nil {
		log.Printf("page %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// qrDataURI renders a QR code of s as an inline PNG.
func qrDataURI(s string) template.URL {
	png, err := qrcode.Encode(s, qrcode.Medium, 256)
	if err != nil {
		return ""
	}
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
}

func fmtTime(t time.Time) string { return t.Local().Format("2 Jan 15:04") }

// fmtUntil says how far in the future t is ("in 1 h 59 min", "expired").
func fmtUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return "in " + fmtDuration(d)
}

// fmtAgo says how long ago t was ("3 min ago", "just now").
func fmtAgo(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	return fmtDuration(d) + " ago"
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d d %d h", days, h)
	case h > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	default:
		return fmt.Sprintf("%d min", m)
	}
}
