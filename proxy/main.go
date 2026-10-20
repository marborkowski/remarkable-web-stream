// rmshare — proxy z hasłem przed goMarkableStream.
//
// Działa na reMarkable. Wystawia formularz logowania; po podaniu poprawnego
// hasła ustawia cookie sesji i przepuszcza cały ruch (w tym długotrwały
// strumień /stream i SSE /events) do goMarkableStream na localhost.
// Cloudflare Tunnel kieruje ruch z internetu na ten proxy.
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
// KONFIGURACJA — zmień hasło tutaj i przebuduj (./deploy.sh)
// ===========================================================================

// Password to hasło wpisywane w przeglądarce. Zahardkodowane celowo.
const Password = "remarkable-live-2026"

// SessionTTL mówi, jak długo cookie po zalogowaniu pozostaje ważne.
const SessionTTL = 30 * 24 * time.Hour

// Ochrona przed zgadywaniem hasła: po tylu nieudanych próbach w oknie
// FailWindow logowanie jest blokowane do końca okna.
const (
	MaxFailures = 20
	FailWindow  = 15 * time.Minute
	FailDelay   = 1500 * time.Millisecond // opóźnienie odpowiedzi po błędnym haśle
)

const cookieName = "rmshare_session"

// ===========================================================================

//go:embed login.html
var loginHTML string

var loginTmpl = template.Must(template.New("login").Parse(loginHTML))

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> wygaśnięcie
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

// locked zwraca true, gdy przekroczono limit nieudanych prób.
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

// isHTTPS mówi, czy klient połączył się przez HTTPS (bezpośrednio lub przez
// Cloudflare Tunnel, który ustawia X-Forwarded-Proto).
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
	listen := flag.String("listen", ":8080", "adres nasłuchu proxy")
	upstream := flag.String("upstream", "http://127.0.0.1:2011", "adres goMarkableStream")
	urlFile := flag.String("urlfile", "/home/root/rmshare/tunnel-url.txt", "plik z aktualnym publicznym adresem tunelu")
	publishDoc := flag.Bool("doc", false, "po zmianie adresu zapisz PDF z adresem i QR w bibliotece tabletu (restartuje xochitl)")
	xochitlDir := flag.String("xochitl-dir", "/home/root/.local/share/remarkable/xochitl", "katalog dokumentów xochitl")
	sleepWatch := flag.Bool("sleep-watch", true, "śledź stan wyświetlacza w journalu xochitl i blokuj streaming, gdy tablet śpi")
	penDevice := flag.String("pen-device", "/dev/input/event2", "urządzenie pióra do zdalnego rysowania z przeglądarki (\"\" = wyłączone)")
	notifyLight := flag.String("notify-light", "/sys/class/backlight/rm_frontlight", "podświetlenie do sygnalizowania widzów mrugnięciem (\"\" = wyłączone)")
	flag.Parse()

	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("zły adres upstream: %v", err)
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
			// Nie przekazujemy cookie sesji dalej — upstream go nie potrzebuje.
			pr.Out.Header.Del("Cookie")
		},
		// -1: każdy fragment odpowiedzi wypychany natychmiast. Konieczne dla
		// strumienia klatek (/stream) i SSE (/events).
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "Strumień z reMarkable jest niedostępny (goMarkableStream nie odpowiada).", http.StatusBadGateway)
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
				renderLogin(w, "Za dużo nieudanych prób. Spróbuj ponownie za kilkanaście minut.", http.StatusTooManyRequests)
				return
			}
			if err := r.ParseForm(); err != nil {
				renderLogin(w, "Nieprawidłowe żądanie.", http.StatusBadRequest)
				return
			}
			given := r.PostFormValue("password")
			if subtle.ConstantTimeCompare([]byte(given), []byte(Password)) != 1 {
				store.recordFailure()
				log.Printf("błędne hasło z %s", clientIP(r))
				time.Sleep(FailDelay)
				renderLogin(w, "Nieprawidłowe hasło.", http.StatusUnauthorized)
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
			log.Printf("zalogowano z %s", clientIP(r))
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
		// Brak WriteTimeout/IdleTimeout: /stream i /events żyją długo.
	}
	log.Printf("rmshare: nasłuch na %s, upstream %s", *listen, target)
	if err := srv.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

// requireAuth przepuszca tylko żądania z ważną sesją. Nawigacja przeglądarki
// bez sesji trafia na formularz; wywołania API/fetch dostają 401.
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
