package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Access model
//
// There are two roles:
//
//   - owner: whoever knows the viewer password (RMSHARE_PASSWORD). The owner
//     watches the screen, draws on the tablet and manages invitations.
//   - guest: whoever opened an invite link. Guests only watch. Their session
//     ends when the invitation expires or is revoked.
//
// An invitation is a random link (/join/<token>) that the owner creates on
// /invites, with a label and a lifetime. Only a hash of the token is stored,
// so the link is shown once, at creation time. Any number of people can join
// through the same link while it is valid.
//
// Sessions and invitations are kept in memory and mirrored to a JSON file, so
// a proxy restart (or a reboot) does not log anyone out. The owner can see
// every active session and end it on /invites.

type role string

const (
	roleOwner role = "owner"
	roleGuest role = "guest"
)

const (
	// OwnerSessionTTL is how long an owner session stays valid after logging
	// in. Guest sessions end when their invitation expires.
	OwnerSessionTTL = 30 * 24 * time.Hour

	// MaxInviteTTL caps the lifetime of an invitation.
	MaxInviteTTL = 30 * 24 * time.Hour

	// Expired invitations stay listed for a while, then they are removed.
	inviteKeepAfterExpiry = 24 * time.Hour

	// A session's last-seen time is written to disk at most this often.
	lastSeenGranularity = time.Minute
)

// Brute-force protection. Wrong passwords and invalid invite links count as
// failures. After MaxFailuresPerIP failures within FailWindow, that client IP
// is blocked until the window has passed. MaxFailuresTotal is a backstop
// across all IPs, because the IP comes from tunnel headers that a client on
// the local network can forge.
const (
	MaxFailuresPerIP = 10
	MaxFailuresTotal = 100
	FailWindow       = 15 * time.Minute
	FailDelay        = 1500 * time.Millisecond // response delay after a failure
)

type invite struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"` // sha256 of the link token
	Label   string    `json:"label"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Joined  int       `json:"joined"` // sessions created through this link
}

func (i *invite) expired(now time.Time) bool { return !now.Before(i.Expires) }

type session struct {
	ID       string    `json:"id"`
	Hash     string    `json:"hash"` // sha256 of the cookie token
	Role     role      `json:"role"`
	Label    string    `json:"label"`            // invite label, or "owner"
	InviteID string    `json:"invite,omitempty"` // guests: the invitation used
	IP       string    `json:"ip"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	LastSeen time.Time `json:"lastSeen"`
	CSRF     string    `json:"csrf"` // per-session token for state-changing forms
}

func (s *session) expired(now time.Time) bool { return !now.Before(s.Expires) }

// canDraw reports whether the session may use the remote pen.
func (s *session) canDraw() bool { return s.Role == roleOwner }

type authState struct {
	Invites  []*invite  `json:"invites"`
	Sessions []*session `json:"sessions"`
}

type authStore struct {
	path string // JSON file; "" = memory only

	mu       sync.Mutex
	invites  map[string]*invite  // by ID
	sessions map[string]*session // by token hash
	failures map[string][]time.Time
	dirty    bool
	now      func() time.Time
}

var (
	errInviteInvalid = errors.New("This invitation is not valid: it has expired, was revoked or is mistyped.")
	errLocked        = errors.New("Too many failed attempts. Try again in a few minutes.")
)

// newAuthStore loads the state file (if any) and starts the background
// housekeeping (expiry, periodic saves).
func newAuthStore(path string) (*authStore, error) {
	s := &authStore{
		path:     path,
		invites:  map[string]*invite{},
		sessions: map[string]*session{},
		failures: map[string][]time.Time{},
		now:      time.Now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	go func() {
		for range time.Tick(time.Minute) {
			s.housekeep()
		}
	}()
	return s, nil
}

func (s *authStore) load() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st authState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	now := s.now()
	for _, i := range st.Invites {
		if i.ID != "" && i.Hash != "" && now.Before(i.Expires.Add(inviteKeepAfterExpiry)) {
			s.invites[i.ID] = i
		}
	}
	for _, sess := range st.Sessions {
		if sess.ID != "" && sess.Hash != "" && !sess.expired(now) {
			s.sessions[sess.Hash] = sess
		}
	}
	return nil
}

// saveLocked writes the state atomically. Requires s.mu.
func (s *authStore) saveLocked() {
	s.dirty = false
	if s.path == "" {
		return
	}
	st := authState{Invites: []*invite{}, Sessions: []*session{}}
	for _, i := range s.invites {
		st.Invites = append(st.Invites, i)
	}
	for _, sess := range s.sessions {
		st.Sessions = append(st.Sessions, sess)
	}
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		log.Printf("auth: cannot encode the state: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		log.Printf("auth: cannot save %s: %v", s.path, err)
	}
}

// housekeep drops expired sessions and old invitations, forgets old failures
// and saves the state if something changed.
func (s *authStore) housekeep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for h, sess := range s.sessions {
		if sess.expired(now) {
			delete(s.sessions, h)
			s.dirty = true
		}
	}
	for id, i := range s.invites {
		if !now.Before(i.Expires.Add(inviteKeepAfterExpiry)) {
			delete(s.invites, id)
			s.dirty = true
		}
	}
	for ip := range s.failures {
		s.pruneFailuresLocked(ip, now)
	}
	if s.dirty {
		s.saveLocked()
	}
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomString(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// createInvite returns the new invitation and the link token, which is not
// stored anywhere and cannot be retrieved later.
func (s *authStore) createInvite(label string, ttl time.Duration) (*invite, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Guest"
	}
	if len(label) > 40 {
		label = label[:40]
	}
	if ttl <= 0 || ttl > MaxInviteTTL {
		return nil, "", fmt.Errorf("invitation lifetime must be between 1 minute and %d days", int(MaxInviteTTL.Hours()/24))
	}
	tok, err := randomString(24)
	if err != nil {
		return nil, "", err
	}
	id, err := newID()
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	inv := &invite{ID: id, Hash: hashToken(tok), Label: label, Created: now, Expires: now.Add(ttl)}
	s.mu.Lock()
	s.invites[id] = inv
	s.saveLocked()
	s.mu.Unlock()
	return inv, tok, nil
}

// findInviteLocked returns the valid invitation for a link token. Requires s.mu.
func (s *authStore) findInviteLocked(tok string) *invite {
	h := hashToken(tok)
	for _, i := range s.invites {
		if i.Hash == h && !i.expired(s.now()) {
			return i
		}
	}
	return nil
}

// lookupInvite returns a copy of the valid invitation behind a link token.
func (s *authStore) lookupInvite(tok string) (invite, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.findInviteLocked(tok); i != nil {
		return *i, true
	}
	return invite{}, false
}

// revokeInvite deletes an invitation and ends every session opened through it.
func (s *authStore) revokeInvite(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.invites[id]; !ok {
		return false
	}
	delete(s.invites, id)
	for h, sess := range s.sessions {
		if sess.InviteID == id {
			delete(s.sessions, h)
		}
	}
	s.saveLocked()
	return true
}

func (s *authStore) listInvites() []invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]invite, 0, len(s.invites))
	for _, i := range s.invites {
		out = append(out, *i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

// newSessionLocked creates a session and returns its cookie token. Requires s.mu.
func (s *authStore) newSessionLocked(r role, label, inviteID, ip string, expires time.Time) (string, *session, error) {
	tok, err := randomString(32)
	if err != nil {
		return "", nil, err
	}
	csrf, err := randomString(18)
	if err != nil {
		return "", nil, err
	}
	id, err := newID()
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	sess := &session{
		ID: id, Hash: hashToken(tok), Role: r, Label: label, InviteID: inviteID,
		IP: ip, Created: now, Expires: expires, LastSeen: now, CSRF: csrf,
	}
	s.sessions[sess.Hash] = sess
	s.saveLocked()
	return tok, sess, nil
}

// loginOwner opens an owner session (the password has been checked by the caller).
func (s *authStore) loginOwner(ip string) (string, *session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newSessionLocked(roleOwner, "owner", "", ip, s.now().Add(OwnerSessionTTL))
}

// join opens a guest session through an invite link. The session lasts as
// long as the invitation.
func (s *authStore) join(tok, ip string) (string, *session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv := s.findInviteLocked(tok)
	if inv == nil {
		return "", nil, errInviteInvalid
	}
	inv.Joined++
	return s.newSessionLocked(roleGuest, inv.Label, inv.ID, ip, inv.Expires)
}

// session returns a copy of the valid session behind a cookie token and
// records that it was seen.
func (s *authStore) session(tok string) (session, bool) {
	if tok == "" {
		return session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[hashToken(tok)]
	if !ok {
		return session{}, false
	}
	now := s.now()
	if sess.expired(now) {
		delete(s.sessions, sess.Hash)
		s.dirty = true
		return session{}, false
	}
	if now.Sub(sess.LastSeen) >= lastSeenGranularity {
		sess.LastSeen = now
		s.dirty = true
	}
	return *sess, true
}

// revokeSession ends the session behind a cookie token (logout).
func (s *authStore) revokeSession(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[hashToken(tok)]; ok {
		delete(s.sessions, hashToken(tok))
		s.saveLocked()
	}
}

// revokeSessionID ends a session by its ID (from the /invites page).
func (s *authStore) revokeSessionID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if sess.ID == id {
			delete(s.sessions, h)
			s.saveLocked()
			return true
		}
	}
	return false
}

func (s *authStore) listSessions() []session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if !sess.expired(now) {
			out = append(out, *sess)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

func (s *authStore) pruneFailuresLocked(ip string, now time.Time) {
	keep := s.failures[ip][:0]
	for _, f := range s.failures[ip] {
		if now.Sub(f) < FailWindow {
			keep = append(keep, f)
		}
	}
	if len(keep) == 0 {
		delete(s.failures, ip)
	} else {
		s.failures[ip] = keep
	}
}

// locked reports whether login attempts from ip are currently blocked.
func (s *authStore) locked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneFailuresLocked(ip, now)
	s.pruneFailuresLocked("", now)
	return len(s.failures[ip]) >= MaxFailuresPerIP || len(s.failures[""]) >= MaxFailuresTotal
}

// recordFailure counts a wrong password or an invalid invite link.
func (s *authStore) recordFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.failures[ip] = append(s.failures[ip], now)
	s.failures[""] = append(s.failures[""], now)
}
