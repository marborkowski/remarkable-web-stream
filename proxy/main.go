// rmshare is a password-protected reverse proxy in front of goMarkableStream.
//
// It runs on the reMarkable itself. It serves a login form; once the correct
// password is entered it sets a session cookie and forwards all traffic
// (including the long-lived /stream frame stream and the /events SSE feed) to
// goMarkableStream on localhost. A tunnel (Tailscale Funnel or Cloudflare
// Tunnel) exposes this proxy to the internet.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "embed"
)

// ===========================================================================
// Configuration
// ===========================================================================

// passwordEnv is the environment variable holding the viewer password. It is
// normally set in tunnel.env, which the systemd unit loads as an
// EnvironmentFile. The -password flag takes precedence.
const passwordEnv = "RMSHARE_PASSWORD"

// minPasswordLen rejects trivially guessable passwords: the proxy is usually
// reachable from the public internet.
const minPasswordLen = 8

// SessionTTL is how long the session cookie stays valid after logging in.
const SessionTTL = 30 * 24 * time.Hour

// Brute-force protection: after MaxFailures failed attempts within
// FailWindow, logging in is blocked until the window has passed.
const (
	MaxFailures = 20
	FailWindow  = 15 * time.Minute
	FailDelay   = 1500 * time.Millisecond // response delay after a wrong password
)

const cookieName = "rmshare_session"

// ===========================================================================

//go:embed login.html
var loginHTML string

var loginTmpl = template.Must(template.New("login").Parse(loginHTML))

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	failures []time.Time
}

func newSessionStore() *sessionStore {
	s := &sessionStore{sessions: map[string]time.Time{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			s.gc()
		}
	}()
	return s
}

func (s *sessionStore) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for t, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, t)
		}
	}
	s.pruneFailuresLocked(now)
}

func (s *sessionStore) pruneFailuresLocked(now time.Time) {
	keep := s.failures[:0]
	for _, f := range s.failures {
		if now.Sub(f) < FailWindow {
			keep = append(keep, f)
		}
	}
	s.failures = keep
}

func (s *sessionStore) create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(SessionTTL)
	s.mu.Unlock()
	return tok, nil
}

func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, tok)
		return false
	}
	return true
}

func (s *sessionStore) revoke(tok string) {
	s.mu.Lock()
	delete(s.sessions, tok)
	s.mu.Unlock()
}

// locked reports whether the failed-attempt limit has been reached.
func (s *sessionStore) locked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneFailuresLocked(time.Now())
	return len(s.failures) >= MaxFailures
}

func (s *sessionStore) recordFailure() {
	s.mu.Lock()
	s.failures = append(s.failures, time.Now())
	s.mu.Unlock()
}

func isAuthenticated(r *http.Request, store *sessionStore) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return store.valid(c.Value)
}

// isHTTPS reports whether the client connected over HTTPS (directly or through
// a tunnel that sets X-Forwarded-Proto).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.Split(ip, ",")[0])
	}
	return r.RemoteAddr
}

func main() {
	listen := flag.String("listen", ":8080", "address the proxy listens on")
	upstream := flag.String("upstream", "http://127.0.0.1:2011", "goMarkableStream address")
	password := flag.String("password", "", "viewer password (default: $"+passwordEnv+")")
	urlFile := flag.String("urlfile", "/home/root/rmshare/tunnel-url.txt", "file holding the current public tunnel URL")
	publishDoc := flag.Bool("doc", false, "when the URL changes, save a PDF with the URL and a QR code to the tablet's library (restarts xochitl)")
	xochitlDir := flag.String("xochitl-dir", "/home/root/.local/share/remarkable/xochitl", "xochitl document directory")
	sleepWatch := flag.Bool("sleep-watch", true, "follow the display state in the xochitl journal and block streaming while the tablet sleeps")
	penDevice := flag.String("pen-device", "/dev/input/event2", "pen input device for remote drawing from the browser (\"\" = disabled)")
	notifyLight := flag.String("notify-light", "/sys/class/backlight/rm_frontlight", "front light used to signal viewers by blinking (\"\" = disabled)")
	flag.Parse()

	if *password == "" {
		*password = os.Getenv(passwordEnv)
	}
	if len(*password) < minPasswordLen {
		log.Fatalf("set a viewer password of at least %d characters with -password or $%s (e.g. in tunnel.env)", minPasswordLen, passwordEnv)
	}

	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	store := newSessionStore()
	urls := &urlState{}
	go watchURLFile(*urlFile, *publishDoc, *xochitlDir, urls)
	sw := newSleepWatcher()
	if *sleepWatch {
		go sw.run("xochitl.service")
	}
	vs := newViewerSignal(*notifyLight, sw.isAwake)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// Do not forward the session cookie: the upstream does not need it.
			pr.Out.Header.Del("Cookie")
		},
		// -1: flush every chunk of the response immediately. Required for the
		// frame stream (/stream) and SSE (/events).
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "The reMarkable stream is unavailable (goMarkableStream is not responding).", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodGet:
			if isAuthenticated(r, store) {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			renderLogin(w, "", http.StatusOK)
		case http.MethodPost:
			if store.locked() {
				renderLogin(w, "Too many failed attempts. Try again in a few minutes.", http.StatusTooManyRequests)
				return
			}
			if err := r.ParseForm(); err != nil {
				renderLogin(w, "Invalid request.", http.StatusBadRequest)
				return
			}
			given := r.PostFormValue("password")
			if subtle.ConstantTimeCompare([]byte(given), []byte(*password)) != 1 {
				store.recordFailure()
				log.Printf("wrong password from %s", clientIP(r))
				time.Sleep(FailDelay)
				renderLogin(w, "Wrong password.", http.StatusUnauthorized)
				return
			}
			tok, err := store.create()
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    tok,
				Path:     "/",
				MaxAge:   int(SessionTTL.Seconds()),
				HttpOnly: true,
				Secure:   isHTTPS(r),
				SameSite: http.SameSiteLaxMode,
			})
			log.Printf("logged in from %s", clientIP(r))
			http.Redirect(w, r, "/", http.StatusSeeOther)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil {
			store.revoke(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	mux.Handle("/state", requireAuth(store, handleState(sw, vs)))
	if *penDevice != "" {
		mux.Handle("/pen", requireAuth(store, handlePen(newPenWriter(*penDevice), sw)))
	}
	mux.Handle("/url", requireAuth(store, handleURL(urls)))
	mux.Handle("/url/qr.png", requireAuth(store, handleURLQR(urls)))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	mux.Handle("/", requireAuth(store, gateSleep(sw, vs.wrap(proxy))))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout/IdleTimeout: /stream and /events are long-lived.
	}
	log.Printf("rmshare: listening on %s, upstream %s", *listen, target)
	if err := srv.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

// requireAuth only lets requests with a valid session through. Browser
// navigation without a session is redirected to the login form; API/fetch
// calls get a 401.
func requireAuth(store *sessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAuthenticated(r, store) {
			if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func renderLogin(w http.ResponseWriter, errMsg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, map[string]string{"Error": errMsg})
}
