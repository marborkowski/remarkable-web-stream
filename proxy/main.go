// rmshare is a password-protected reverse proxy in front of goMarkableStream.
//
// It runs on the reMarkable itself. It serves a login form; once the correct
// password is entered it sets a session cookie and forwards all traffic
// (including the long-lived /stream frame stream and the /events SSE feed) to
// goMarkableStream on localhost. A tunnel (Tailscale Funnel or Cloudflare
// Tunnel) exposes this proxy to the internet.
//
// Besides the owner's password, the proxy knows invite links: see auth.go.
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// ===========================================================================
// Configuration
// ===========================================================================

// passwordEnv is the environment variable holding the owner password. It is
// normally set in tunnel.env, which the systemd unit loads as an
// EnvironmentFile. The -password flag takes precedence.
const passwordEnv = "RMSHARE_PASSWORD"

// minPasswordLen rejects trivially guessable passwords: the proxy is usually
// reachable from the public internet.
const minPasswordLen = 8

const cookieName = "rmshare_session"

// ===========================================================================

type server struct {
	password string
	store    *authStore
	urls     *urlState
	sw       *sleepWatcher
	vs       *viewerSignal
	pen      *penWriter // nil = remote pen disabled
	proxy    http.Handler
}

func main() {
	listen := flag.String("listen", ":8080", "address the proxy listens on")
	upstream := flag.String("upstream", "http://127.0.0.1:2011", "goMarkableStream address")
	password := flag.String("password", "", "owner password (default: $"+passwordEnv+")")
	stateFile := flag.String("state-file", "/home/root/rmshare/state.json", "file keeping sessions and invitations across restarts (\"\" = memory only)")
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
		log.Fatalf("set an owner password of at least %d characters with -password or $%s (e.g. in tunnel.env)", minPasswordLen, passwordEnv)
	}

	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	store, err := newAuthStore(*stateFile)
	if err != nil {
		log.Fatalf("cannot load the session state: %v", err)
	}
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

	s := &server{password: *password, store: store, urls: urls, sw: sw, vs: vs, proxy: proxy}
	if *penDevice != "" {
		s.pen = newPenWriter(*penDevice)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout/IdleTimeout: /stream and /events are long-lived.
	}
	log.Printf("rmshare: listening on %s, upstream %s", *listen, target)
	if err := srv.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

// handler builds the route table.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("GET /join/{token}", s.joinPage)
	mux.HandleFunc("POST /join", s.joinPost)
	mux.HandleFunc("/logout", s.logout)
	mux.Handle("GET /invites", s.requireRole(roleOwner, http.HandlerFunc(s.invitesPage)))
	mux.Handle("POST /invites", s.requireRole(roleOwner, http.HandlerFunc(s.invitesPost)))

	mux.Handle("/state", s.requireAuth(handleState(s.sw, s.vs, s.pen != nil)))
	if s.pen != nil {
		mux.Handle("/pen", s.requireRole(roleOwner, handlePen(s.pen, s.sw)))
	}
	mux.Handle("/url", s.requireAuth(handleURL(s.urls)))
	mux.Handle("/url/qr.png", s.requireAuth(handleURLQR(s.urls)))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	mux.Handle("/", s.requireAuth(gateSleep(s.sw, s.vs.wrap(s.proxy))))
	return mux
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

type sessionKey struct{}

// sessionFrom returns the session that requireAuth attached to the request.
func sessionFrom(r *http.Request) (session, bool) {
	sess, ok := r.Context().Value(sessionKey{}).(session)
	return sess, ok
}

func (s *server) currentSession(r *http.Request) (session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return session{}, false
	}
	return s.store.session(c.Value)
}

// requireAuth only lets requests with a valid session through and attaches
// the session to the request. Browser navigation without a session is
// redirected to the login form; API/fetch calls get a 401.
func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentSession(r)
		if !ok {
			if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
	})
}

// requireRole is requireAuth plus a role check (403 for the wrong role).
func (s *server) requireRole(want role, next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess, _ := sessionFrom(r); sess.Role != want {
			http.Error(w, "forbidden: this needs the "+string(want)+" role", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *server) setSessionCookie(w http.ResponseWriter, r *http.Request, tok string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
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
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// sameOriginPost rejects form posts whose Origin (or Referer) belongs to
// another site. Browsers always send one of them with a POST.
func sameOriginPost(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true // non-browser client; the session cookie is still required
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}

// ---------------------------------------------------------------------------
// Login (owner), join (guest), logout
// ---------------------------------------------------------------------------

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if sess, ok := s.currentSession(r); ok && sess.Role == roleOwner {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	renderPage(w, "login", http.StatusOK, pageData{})
}

func (s *server) loginPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := clientIP(r)
	if s.store.locked(ip) {
		renderPage(w, "login", http.StatusTooManyRequests, pageData{Error: errLocked.Error()})
		return
	}
	if err := r.ParseForm(); err != nil {
		renderPage(w, "login", http.StatusBadRequest, pageData{Error: "Invalid request."})
		return
	}
	given := r.PostFormValue("password")
	if subtle.ConstantTimeCompare([]byte(given), []byte(s.password)) != 1 {
		s.store.recordFailure(ip)
		log.Printf("wrong password from %s", ip)
		time.Sleep(FailDelay)
		renderPage(w, "login", http.StatusUnauthorized, pageData{Error: "Wrong password."})
		return
	}
	tok, sess, err := s.store.loginOwner(ip)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Signing in replaces any (guest) session this browser already had.
	if c, err := r.Cookie(cookieName); err == nil {
		s.store.revokeSession(c.Value)
	}
	s.setSessionCookie(w, r, tok, sess.Expires)
	log.Printf("owner logged in from %s", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// joinPage shows what the invite link gives access to, with a Join button.
// The session is only created by the POST, so link previews and prefetchers
// do not open sessions.
func (s *server) joinPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Keep the link token out of Referer headers sent to other sites. (With
	// "no-referrer" browsers would also send "Origin: null" with the form
	// post, which sameOriginPost rejects.)
	w.Header().Set("Referrer-Policy", "same-origin")
	ip := clientIP(r)
	if s.store.locked(ip) {
		renderPage(w, "join", http.StatusTooManyRequests, pageData{Error: errLocked.Error()})
		return
	}
	tok := r.PathValue("token")
	inv, ok := s.store.lookupInvite(tok)
	if !ok {
		s.store.recordFailure(ip)
		log.Printf("invalid invite link from %s", ip)
		time.Sleep(FailDelay)
		renderPage(w, "join", http.StatusNotFound, pageData{Error: errInviteInvalid.Error()})
		return
	}
	cur, signedIn := s.currentSession(r)
	renderPage(w, "join", http.StatusOK, pageData{
		Token: tok, Invite: &inv, SignedIn: signedIn, Session: cur,
	})
}

func (s *server) joinPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := clientIP(r)
	if !sameOriginPost(r) {
		http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
		return
	}
	if s.store.locked(ip) {
		renderPage(w, "join", http.StatusTooManyRequests, pageData{Error: errLocked.Error()})
		return
	}
	if err := r.ParseForm(); err != nil {
		renderPage(w, "join", http.StatusBadRequest, pageData{Error: "Invalid request."})
		return
	}
	tok, sess, err := s.store.join(r.PostFormValue("token"), ip)
	if err != nil {
		s.store.recordFailure(ip)
		log.Printf("invalid invite link from %s", ip)
		time.Sleep(FailDelay)
		renderPage(w, "join", http.StatusNotFound, pageData{Error: errInviteInvalid.Error()})
		return
	}
	// Joining replaces any session this browser already had.
	if c, err := r.Cookie(cookieName); err == nil {
		s.store.revokeSession(c.Value)
	}
	s.setSessionCookie(w, r, tok, sess.Expires)
	log.Printf("guest %q joined from %s (invitation %s)", sess.Label, ip, sess.InviteID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.store.revokeSession(c.Value)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Invitations (owner only)
// ---------------------------------------------------------------------------

// inviteLifetimes are the choices offered on the /invites page.
var inviteLifetimes = []struct {
	Label string
	D     time.Duration
}{
	{"1 hour", time.Hour},
	{"2 hours", 2 * time.Hour},
	{"4 hours", 4 * time.Hour},
	{"8 hours", 8 * time.Hour},
	{"24 hours", 24 * time.Hour},
	{"7 days", 7 * 24 * time.Hour},
	{"30 days", 30 * 24 * time.Hour},
}

const defaultInviteLifetime = 2 * time.Hour

func (s *server) invitesPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.renderInvites(w, r, http.StatusOK, pageData{})
}

func (s *server) renderInvites(w http.ResponseWriter, r *http.Request, status int, d pageData) {
	d.Session, _ = sessionFrom(r)
	d.Invites = s.store.listInvites()
	d.Sessions = s.store.listSessions()
	d.Lifetimes = inviteLifetimes
	d.DefaultLifetime = defaultInviteLifetime
	renderPage(w, "invites", status, d)
}

func (s *server) invitesPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sess, _ := sessionFrom(r)
	if !sameOriginPost(r) {
		http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderInvites(w, r, http.StatusBadRequest, pageData{Error: "Invalid request."})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
		s.renderInvites(w, r, http.StatusForbidden, pageData{Error: "The form has expired. Please try again."})
		return
	}
	switch r.PostFormValue("action") {
	case "create":
		ttl, err := time.ParseDuration(r.PostFormValue("lifetime"))
		if err != nil {
			ttl = defaultInviteLifetime
		}
		inv, tok, err := s.store.createInvite(r.PostFormValue("label"), ttl)
		if err != nil {
			s.renderInvites(w, r, http.StatusBadRequest, pageData{Error: err.Error()})
			return
		}
		link := s.joinLink(r, tok)
		log.Printf("invitation %q created by the owner (%s), valid for %s", inv.Label, clientIP(r), ttl)
		s.renderInvites(w, r, http.StatusOK, pageData{NewInvite: inv, NewLink: link, NewLinkQR: qrDataURI(link)})
	case "revoke":
		if s.store.revokeInvite(r.PostFormValue("id")) {
			log.Printf("invitation %s revoked by the owner (%s)", r.PostFormValue("id"), clientIP(r))
		}
		http.Redirect(w, r, "/invites", http.StatusSeeOther)
	case "end-session":
		id := r.PostFormValue("id")
		if id == sess.ID {
			s.logout(w, r)
			return
		}
		if s.store.revokeSessionID(id) {
			log.Printf("session %s ended by the owner (%s)", id, clientIP(r))
		}
		http.Redirect(w, r, "/invites", http.StatusSeeOther)
	default:
		s.renderInvites(w, r, http.StatusBadRequest, pageData{Error: "Unknown action."})
	}
}

// joinLink builds the invite URL from the address the owner is using right now.
func (s *server) joinLink(r *http.Request, tok string) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/join/" + tok
}
