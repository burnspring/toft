package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeVersions struct {
	versions map[string]int
	locked   map[string]bool
	err      error
}

func (f *fakeVersions) GetSessionState(userID string) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	v, ok := f.versions[userID]
	if !ok {
		return 0, false, errors.New("no such user")
	}
	return v, f.locked[userID], nil
}

// principalSeenBy runs a request with the given cookie through Middleware and
// returns the principal the inner handler observed.
func principalSeenBy(t *testing.T, states SessionStates, token string) *Principal {
	t.Helper()

	var got *Principal
	h := Middleware(NewJWTService("test-secret-key-32-bytes-long!!!"), states, Options{})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = GetPrincipal(r.Context())
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// A token minted at an older token_version is revoked: the request proceeds
// anonymously. Regression test for hardening task 1.5.
func TestMiddlewareRejectsStaleTokenVersion(t *testing.T) {
	jwtSvc := NewJWTService("test-secret-key-32-bytes-long!!!")
	sess, err := jwtSvc.Generate(GenerateParams{UserID: "u1", SystemRole: "system_admin", TokenVersion: 3, Policy: DefaultSessionPolicy()})
	if err != nil {
		t.Fatal(err)
	}

	if p := principalSeenBy(t, &fakeVersions{versions: map[string]int{"u1": 3}}, sess.Token); p.IsAnonymous() {
		t.Fatal("matching token_version should authenticate")
	}
	if p := principalSeenBy(t, &fakeVersions{versions: map[string]int{"u1": 4}}, sess.Token); !p.IsAnonymous() {
		t.Fatal("stale token_version must yield an anonymous principal")
	}
}

// A locked user's live sessions die on the next request, even with a
// matching token_version (access-control plan WS-B).
func TestMiddlewareRejectsLockedUser(t *testing.T) {
	jwtSvc := NewJWTService("test-secret-key-32-bytes-long!!!")
	sess, err := jwtSvc.Generate(GenerateParams{UserID: "u1", SystemRole: "publisher", TokenVersion: 3, Policy: DefaultSessionPolicy()})
	if err != nil {
		t.Fatal(err)
	}

	states := &fakeVersions{versions: map[string]int{"u1": 3}, locked: map[string]bool{"u1": true}}
	if p := principalSeenBy(t, states, sess.Token); !p.IsAnonymous() {
		t.Fatal("locked user must yield an anonymous principal")
	}
}

// Version lookup failures (user deleted, database down) fail closed.
func TestMiddlewareFailsClosedOnVersionLookupError(t *testing.T) {
	jwtSvc := NewJWTService("test-secret-key-32-bytes-long!!!")
	sess, err := jwtSvc.Generate(GenerateParams{UserID: "ghost", SystemRole: "system_admin", Policy: DefaultSessionPolicy()})
	if err != nil {
		t.Fatal(err)
	}

	if p := principalSeenBy(t, &fakeVersions{err: errors.New("db down")}, sess.Token); !p.IsAnonymous() {
		t.Fatal("lookup error must yield an anonymous principal")
	}
	if p := principalSeenBy(t, nil, sess.Token); !p.IsAnonymous() {
		t.Fatal("nil versions source must yield an anonymous principal")
	}
}
