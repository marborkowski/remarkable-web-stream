package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, path string) *authStore {
	t.Helper()
	s := &authStore{
		path:     path,
		invites:  map[string]*invite{},
		sessions: map[string]*session{},
		failures: map[string][]time.Time{},
		now:      time.Now,
	}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInviteJoinRevoke(t *testing.T) {
	s := newTestStore(t, "")
	inv, tok, err := s.createInvite("  Anna ", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Label != "Anna" {
		t.Errorf("label %q", inv.Label)
	}
	if _, ok := s.lookupInvite(tok); !ok {
		t.Fatal("fresh invite not found")
	}
	if _, ok := s.lookupInvite(tok + "x"); ok {
		t.Fatal("wrong token accepted")
	}

	st, sess, err := s.join(tok, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Role != roleGuest || sess.Label != "Anna" || sess.InviteID != inv.ID || !sess.Expires.Equal(inv.Expires) {
		t.Errorf("unexpected guest session %+v", sess)
	}
	if got, ok := s.session(st); !ok || got.ID != sess.ID {
		t.Fatal("guest session not valid")
	}
	if s.listInvites()[0].Joined != 1 {
		t.Errorf("joined count not incremented")
	}

	if !s.revokeInvite(inv.ID) {
		t.Fatal("revoke failed")
	}
	if _, ok := s.session(st); ok {
		t.Fatal("guest session survived the revocation of its invitation")
	}
	if _, _, err := s.join(tok, "1.2.3.4"); err == nil {
		t.Fatal("joining through a revoked invitation succeeded")
	}
}

func TestInviteExpiry(t *testing.T) {
	s := newTestStore(t, "")
	now := time.Now()
	s.now = func() time.Time { return now }
	_, tok, err := s.createInvite("x", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := s.join(tok, "ip")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Minute)
	if _, ok := s.session(st); ok {
		t.Error("guest session valid after the invitation expired")
	}
	if _, _, err := s.join(tok, "ip"); err == nil {
		t.Error("expired invitation accepted")
	}
	now = now.Add(inviteKeepAfterExpiry)
	s.housekeep()
	if len(s.listInvites()) != 0 {
		t.Error("expired invitation not cleaned up")
	}
	if _, _, err := s.createInvite("x", MaxInviteTTL+time.Hour); err == nil {
		t.Error("lifetime above the maximum accepted")
	}
}

func TestOwnerSessionAndLogout(t *testing.T) {
	s := newTestStore(t, "")
	tok, sess, err := s.loginOwner("ip")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.session(tok)
	if !ok || got.Role != roleOwner || !got.canDraw() || got.CSRF == "" {
		t.Fatalf("owner session %+v ok=%v", got, ok)
	}
	if !s.revokeSessionID(sess.ID) {
		t.Fatal("revoke by id failed")
	}
	if _, ok := s.session(tok); ok {
		t.Fatal("session valid after revocation")
	}
	s.revokeSession(tok) // no-op, must not panic
}

func TestStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := newTestStore(t, path)
	ownerTok, _, err := s.loginOwner("a")
	if err != nil {
		t.Fatal(err)
	}
	inv, invTok, err := s.createInvite("Bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	guestTok, _, err := s.join(invTok, "b")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, mode %v", err, fi.Mode())
	}
	b, _ := os.ReadFile(path)
	for _, secret := range []string{ownerTok, invTok, guestTok} {
		if strings.Contains(string(b), secret) {
			t.Fatal("a plain token was written to the state file")
		}
	}

	// A new process loads the same state.
	s2 := newTestStore(t, path)
	if sess, ok := s2.session(ownerTok); !ok || sess.Role != roleOwner {
		t.Error("owner session not restored")
	}
	if sess, ok := s2.session(guestTok); !ok || sess.Role != roleGuest || sess.InviteID != inv.ID {
		t.Error("guest session not restored")
	}
	if _, ok := s2.lookupInvite(invTok); !ok {
		t.Error("invitation not restored")
	}
	if !s2.revokeInvite(inv.ID) {
		t.Fatal("revoke failed")
	}
	s3 := newTestStore(t, path)
	if _, ok := s3.session(guestTok); ok {
		t.Error("revocation not persisted")
	}
	if _, ok := s3.session(ownerTok); !ok {
		t.Error("owner session lost")
	}
}

func TestLockoutPerIP(t *testing.T) {
	s := newTestStore(t, "")
	for i := 0; i < MaxFailuresPerIP-1; i++ {
		s.recordFailure("a")
	}
	if s.locked("a") {
		t.Fatal("locked too early")
	}
	s.recordFailure("a")
	if !s.locked("a") {
		t.Fatal("not locked after the limit")
	}
	if s.locked("b") {
		t.Fatal("another IP was locked out")
	}
	// The window passes: unlocked again.
	now := time.Now()
	s.now = func() time.Time { return now.Add(FailWindow + time.Second) }
	if s.locked("a") {
		t.Fatal("still locked after the window")
	}
	// Global backstop across many IPs.
	for i := 0; i < MaxFailuresTotal; i++ {
		s.recordFailure("ip" + itoa(i))
	}
	if !s.locked("fresh") {
		t.Fatal("global backstop did not lock")
	}
}

// --- HTTP flow -------------------------------------------------------------

func newTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	pw, _ := newTestPen(t)
	s := &server{
		password: "correct-horse-battery",
		store:    newTestStore(t, ""),
		urls:     &urlState{},
		sw:       newSleepWatcher(),
		vs:       newViewerSignal("", nil),
		pen:      pw,
		proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "upstream "+r.URL.Path)
		}),
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// client returns an HTTP client with a cookie jar that does not follow redirects.
func client(t *testing.T) *http.Client {
	t.Helper()
	jar := &cookieJar{}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type cookieJar struct{ cookies []*http.Cookie }

// SetCookies replaces a cookie of the same name, like a browser does.
func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	for _, c := range cs {
		keep := j.cookies[:0]
		for _, old := range j.cookies {
			if old.Name != c.Name {
				keep = append(keep, old)
			}
		}
		j.cookies = keep
		if c.MaxAge >= 0 {
			j.cookies = append(j.cookies, c)
		}
	}
}
func (j *cookieJar) Cookies(*url.URL) []*http.Cookie { return j.cookies }

func postForm(t *testing.T, c *http.Client, u string, form url.Values, origin string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func get(t *testing.T, c *http.Client, u string, html bool) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if html {
		req.Header.Set("Accept", "text/html")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestHTTPOwnerFlow(t *testing.T) {
	s, ts := newTestServer(t)
	c := client(t)

	if resp, _ := get(t, c, ts.URL+"/", true); resp.StatusCode != 302 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous browser GET /: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := get(t, c, ts.URL+"/state", false); resp.StatusCode != 401 {
		t.Fatalf("anonymous /state: %d", resp.StatusCode)
	}
	if resp := postForm(t, c, ts.URL+"/login", url.Values{"password": {"wrong-password"}}, ""); resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	if resp := postForm(t, c, ts.URL+"/login", url.Values{"password": {"correct-horse-battery"}}, ""); resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if resp, body := get(t, c, ts.URL+"/", true); resp.StatusCode != 200 || body != "upstream /" {
		t.Fatalf("owner GET /: %d %q", resp.StatusCode, body)
	}
	if _, body := get(t, c, ts.URL+"/state", false); !strings.Contains(body, `"role":"owner"`) || !strings.Contains(body, `"canDraw":true`) {
		t.Fatalf("owner /state: %s", body)
	}

	// Invitations page and creating a link.
	resp, body := get(t, c, ts.URL+"/invites", true)
	if resp.StatusCode != 200 {
		t.Fatalf("/invites: %d", resp.StatusCode)
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if csrf == nil {
		t.Fatal("no csrf token on /invites")
	}
	form := url.Values{"csrf": {csrf[1]}, "action": {"create"}, "label": {"Anna"}, "lifetime": {"1h0m0s"}}
	if resp := postForm(t, c, ts.URL+"/invites", form, "https://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("cross-site create: %d", resp.StatusCode)
	}
	bad := url.Values{"csrf": {"nope"}, "action": {"create"}}
	if resp := postForm(t, c, ts.URL+"/invites", bad, ""); resp.StatusCode != 403 {
		t.Fatalf("bad csrf: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/invites", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	link := regexp.MustCompile(`id="newLink" value="([^"]+)"`).FindStringSubmatch(string(b))
	if r2.StatusCode != 200 || link == nil {
		t.Fatalf("create invite: %d %s", r2.StatusCode, b)
	}
	if !strings.HasPrefix(link[1], ts.URL+"/join/") {
		t.Fatalf("link %q", link[1])
	}

	// A guest joins through the link.
	g := client(t)
	resp, body = get(t, g, link[1], true)
	if resp.StatusCode != 200 || !strings.Contains(body, "Anna") {
		t.Fatalf("join page: %d %s", resp.StatusCode, body)
	}
	tok := strings.TrimPrefix(link[1], ts.URL+"/join/")
	if resp := postForm(t, g, ts.URL+"/join", url.Values{"token": {tok}}, ts.URL); resp.StatusCode != 303 {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	if resp, body := get(t, g, ts.URL+"/", true); resp.StatusCode != 200 || body != "upstream /" {
		t.Fatalf("guest GET /: %d %q", resp.StatusCode, body)
	}
	if _, body := get(t, g, ts.URL+"/state", false); !strings.Contains(body, `"role":"guest"`) || !strings.Contains(body, `"canDraw":false`) {
		t.Fatalf("guest /state: %s", body)
	}
	if resp, _ := get(t, g, ts.URL+"/pen", false); resp.StatusCode != 403 {
		t.Fatalf("guest /pen: %d", resp.StatusCode)
	}
	if resp, _ := get(t, g, ts.URL+"/invites", true); resp.StatusCode != 403 {
		t.Fatalf("guest /invites: %d", resp.StatusCode)
	}

	// The owner revokes the invitation: the guest is out.
	_, body = get(t, c, ts.URL+"/invites", true)
	id := regexp.MustCompile(`name="action" value="revoke"><input type="hidden" name="id" value="([^"]+)"`).FindStringSubmatch(body)
	if id == nil {
		t.Fatal("no invitation listed")
	}
	if resp := postForm(t, c, ts.URL+"/invites", url.Values{"csrf": {csrf[1]}, "action": {"revoke"}, "id": {id[1]}}, ts.URL); resp.StatusCode != 303 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if resp, _ := get(t, g, ts.URL+"/state", false); resp.StatusCode != 401 {
		t.Fatalf("guest after revocation: %d", resp.StatusCode)
	}
	if resp, _ := get(t, g, link[1], true); resp.StatusCode != 404 {
		t.Fatalf("revoked link: %d", resp.StatusCode)
	}

	// A guest can sign in with the password and becomes the owner.
	if resp := postForm(t, g, ts.URL+"/join", url.Values{"token": {tok}}, ts.URL); resp.StatusCode != 404 {
		t.Fatalf("re-join through the revoked link: %d", resp.StatusCode)
	}
	g2 := client(t)
	_, invTok2, _ := s.store.createInvite("Bob", time.Hour)
	if resp := postForm(t, g2, ts.URL+"/join", url.Values{"token": {invTok2}}, ts.URL); resp.StatusCode != 303 {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	if resp, _ := get(t, g2, ts.URL+"/login", true); resp.StatusCode != 200 {
		t.Fatalf("login page for a guest: %d", resp.StatusCode)
	}
	if resp := postForm(t, g2, ts.URL+"/login", url.Values{"password": {"correct-horse-battery"}}, ""); resp.StatusCode != 303 {
		t.Fatalf("guest login: %d", resp.StatusCode)
	}
	if _, body := get(t, g2, ts.URL+"/state", false); !strings.Contains(body, `"role":"owner"`) {
		t.Fatalf("guest upgraded to owner: %s", body)
	}
	if n := len(s.store.listSessions()); n != 2 {
		t.Fatalf("sessions after upgrade: %d, want 2 (old guest session replaced)", n)
	}
	if resp, _ := get(t, c, ts.URL+"/login", true); resp.StatusCode != 302 {
		t.Fatalf("login page for the owner: %d", resp.StatusCode)
	}

	// Logout.
	if resp, _ := get(t, c, ts.URL+"/logout", true); resp.StatusCode != 303 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, ts.URL+"/state", false); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.11.99.2:51234"
	if got := clientIP(r); got != "10.11.99.2" {
		t.Errorf("RemoteAddr: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("X-Forwarded-For: %q", got)
	}
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	if got := clientIP(r); got != "198.51.100.7" {
		t.Errorf("CF-Connecting-IP: %q", got)
	}
}

func TestHTTPLockout(t *testing.T) {
	s, ts := newTestServer(t)
	c := client(t)
	for i := 0; i < MaxFailuresPerIP; i++ {
		s.store.recordFailure("9.9.9.9")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader("password=correct-horse-battery"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("locked IP with the right password: %d", resp.StatusCode)
	}
	// Other clients are not affected.
	if resp := postForm(t, client(t), ts.URL+"/login", url.Values{"password": {"correct-horse-battery"}}, ""); resp.StatusCode != 303 {
		t.Fatalf("unlocked client: %d", resp.StatusCode)
	}
}
