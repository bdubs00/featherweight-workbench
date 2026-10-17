// Package web serves the workbench for people on the native surface of the
// kernel: the page at /ui, the browser sign-in, the home, the result of a
// run, the transcript, the builder, the fleet, the deprecated usage, and the
// connections of a person to services. It mounts its routes as an
// extension of package native, on the same listener and behind the same
// authentication as the run routes.
package web

import (
	"net/http"
	"slices"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

func init() { native.Extend(newExtension(time.Now)) }

// newExtension returns the extension with a clock, so that a test sets the
// time.
func newExtension(now func() time.Time) native.Extension {
	return func(k *native.Kit) error {
		s := &server{k: k, a: k.Agent(), now: now}
		return s.mount()
	}
}

type server struct {
	k        *native.Kit
	a        *fw.Agent
	now      func() time.Time
	signin   *signin // nil when browser sign-in is off
	connects *connects
}

func (s *server) mount() error {
	si, err := newSignin(s.a, s.now)
	if err != nil {
		return err
	}
	s.signin = si
	s.connects = &connects{s: s}
	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET /v1/home", s.home},
		{"POST /v1/builder", s.builder},
		{"GET /v1/runs/{id}/result", s.k.Remote(s.result)},
		{"GET /v1/runs/{id}/transcript", s.k.Remote(s.transcript)},
		{"GET /v1/fleet", s.fleet},
		{"GET /v1/usage", s.usage},
		{"GET /v1/connections", s.listConnections},
		{"POST /v1/connections/{server}", s.startConnection},
		{"DELETE /v1/connections/{server}", s.deleteConnection},
	}
	for _, rt := range routes {
		s.k.Handle(rt.pattern, rt.h)
	}
	// The page needs no credential: it authenticates to /v1 with the token
	// of the browser sign-in, or with a token that the person pastes into
	// it.
	s.k.HandlePublic("GET /ui", s.ui)
	if s.signin != nil {
		// Browser sign-in: the page sends the person here, and the issuer
		// sends the person back to the callback.
		s.k.SetSigninPath(signinPath)
		s.k.HandlePublic("GET /ui/signin", s.signinStart)
		s.k.HandlePublic("GET /ui/callback", s.signinCallback)
	}
	// The end of a connection: the authorization server sends the person
	// here. The connect cookie says who started it.
	s.k.HandlePublic("GET "+connectCallbackPath, s.connectCallback)
	if slices.Contains(s.a.Config().Auth.Modes(), "oidc") {
		// RFC 9728 puts the path of the resource after the well-known path,
		// so the resource /mcp has its document at
		// /.well-known/oauth-protected-resource/mcp. Both answer the same.
		s.k.HandlePublic("GET /.well-known/oauth-protected-resource", s.protectedResource)
		s.k.HandlePublic("GET /.well-known/oauth-protected-resource/{rest...}", s.protectedResource)
	}
	return nil
}

// The helpers of the run routes, under the names that the handlers of this
// package use.

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) { s.k.Fail(w, r, err) }

func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return s.k.Decode(w, r, v)
}

func (s *server) decodeFiles(w http.ResponseWriter, r *http.Request, v any, files func() int) bool {
	return s.k.DecodeFiles(w, r, v, files)
}

func (s *server) start(w http.ResponseWriter, r *http.Request, req fw.RunRequest) (*fw.Run, bool) {
	return s.k.Start(w, r, req)
}

func (s *server) ownerOf(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	return s.k.OwnerOf(w, r, id)
}

func (s *server) forward(w http.ResponseWriter, r *http.Request, member string) {
	s.k.Forward(w, r, member)
}

func problem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	native.WriteProblem(w, r, status, detail)
}

func writeJSON(w http.ResponseWriter, status int, v any) { native.WriteJSON(w, status, v) }

func accepts(r *http.Request, mediaType string) bool { return native.Accepts(r, mediaType) }

func cause(err error) string { return native.Cause(err) }

type fileBody = native.FileBody

func attachments(files []fileBody) []fw.Block { return native.Attachments(files) }

const (
	defaultEventsLimit = native.DefaultEventsLimit
	maxEventsLimit     = native.MaxEventsLimit
)
