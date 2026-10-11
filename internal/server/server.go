// Package server opens the agent's listeners, wraps them in TLS when
// configured, and applies the fixed middleware chain (recover, observe,
// access control, authenticate, concurrency limit) in front of a handler.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"

	"github.com/krisiasty/hwi-api/internal/auth"
	"github.com/krisiasty/hwi-api/internal/config"
)

// maxHeaderBytes bounds the size of request headers a listener will read,
// independent of any configured timeout.
const maxHeaderBytes = 64 * 1024

// aeadCipherSuitesTLS12 is the AEAD-only suite list pinned when the
// configured floor lowers TLS to 1.2. TLS 1.3 has no cipher suite knob to
// configure here: Go does not allow selecting its suites, all of which are
// already AEAD.
var aeadCipherSuitesTLS12 = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
}

// BuildTLSConfig loads the listener's certificate and key (resolving any
// credential: reference through config.ResolvePath) and builds the
// *tls.Config a Server should serve with.
//
// a is nil when the caller wants a TLS config with no client-certificate
// involvement at all (for example while composing a test in isolation); in
// that case, and whenever a.MTLSEnabled() is false, ClientCAs is left nil
// and ClientAuth is tls.NoClientCert, since crypto/tls populates
// ConnectionState.VerifiedChains on the server side only when ClientAuth is
// VerifyClientCertIfGiven or RequireAndVerifyClientCert - never for
// NoClientCert - so leaving it at its zero value is the correct "mTLS plays
// no part here" configuration, not an oversight.
//
// When a.MTLSEnabled(), ClientCAs is set from a.ClientCAs() so the
// handshake actually verifies a presented certificate against the
// configured pool; without that assignment, RequireAndVerifyClientCert or
// VerifyClientCertIfGiven would still be set below, but crypto/tls would
// have no pool to verify against and every handshake requiring or
// accepting a certificate would fail (or, worse, verify against the system
// root pool instead of the configured one).
func BuildTLSConfig(cfg config.TLSConfig, a *auth.Authenticator) (*tls.Config, error) {
	// Read through config.ReadFile rather than tls.LoadX509KeyPair, whose
	// read errors carry the resolved path: for a credential that path ends
	// in its name. The field prefixes identify which file failed instead.
	certPEM, err := config.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("tls.cert_file: %w", err)
	}
	keyPEM, err := config.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls.key_file: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading TLS key pair: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	switch cfg.MinVersion {
	case "1.2":
		tlsCfg.MinVersion = tls.VersionTLS12
		tlsCfg.CipherSuites = append([]uint16(nil), aeadCipherSuitesTLS12...)
	default:
		// "1.3", and any other value: config.Validate already rejects
		// anything but "1.2" or "1.3" before this is ever called, so this
		// is TLS 1.3, the default floor.
		tlsCfg.MinVersion = tls.VersionTLS13
	}

	if a != nil && a.MTLSEnabled() {
		tlsCfg.ClientCAs = a.ClientCAs()

		// THE critical invariant: only these two ClientAuthType values
		// cause crypto/tls to populate ConnectionState.VerifiedChains on
		// the server side at all, which is what internal/auth.Authenticate
		// trusts absolutely for MethodMTLS. Getting this wrong - or
		// forgetting ClientCAs above - is how the whole mTLS path becomes
		// decorative while still appearing to work.
		//
		// The choice between the two is safe in both directions, which is
		// what makes lowering it to VerifyClientCertIfGiven acceptable
		// when a token-only client must also be able to connect:
		// VerifyClientCertIfGiven lets a certificate-less client complete
		// the handshake at all, but a client that *does* present one still
		// has it verified against ClientCAs during the handshake - so
		// VerifiedChains ends up non-empty only after real verification
		// either way, and internal/auth's trust in it holds under both
		// modes. The only thing that changes is whether a client with no
		// certificate and no valid token can reach the handler at all
		// (rejected at the TLS layer under RequireAndVerifyClientCert,
		// rejected by auth.Authenticate's own ErrNoCredentials/ErrRejected
		// under VerifyClientCertIfGiven) - never whether an unverified
		// certificate can pass as verified.
		if a.TokenEnabled() {
			tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
		} else {
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}

	return tlsCfg, nil
}

// Options configures a Server.
type Options struct {
	// Name identifies the listener in logs and metrics (the "listener"
	// label), e.g. "api" or "ops".
	Name string
	// Addrs is the set of addresses to bind, as netbind.Resolve returns
	// them: a single invalid netip.Addr means "every address".
	Addrs []netip.Addr
	Port  int
	// TLS is served over every address when non-nil; a plain listener is
	// used when nil.
	TLS      *tls.Config
	Timeouts config.TimeoutConfig
	Handler  http.Handler
	Log      *slog.Logger
}

// Server serves one logical listener - possibly bound to several
// addresses, sharing one http.Server configuration and one Handler.
type Server struct {
	name    string
	addrs   []netip.Addr
	port    int
	tlsCfg  *tls.Config
	log     *slog.Logger
	httpSrv *http.Server

	mu        sync.Mutex
	listeners []net.Listener

	// bound is closed once Serve has opened every listener; see Bound.
	bound chan struct{}
}

// New builds a Server from Options. It opens no listener and binds nothing;
// call Serve to do that.
func New(o Options) (*Server, error) {
	if o.Handler == nil {
		return nil, errors.New("server: Handler is required")
	}
	if o.Log == nil {
		return nil, errors.New("server: Log is required")
	}
	if len(o.Addrs) == 0 {
		return nil, errors.New("server: at least one bind address is required")
	}

	s := &Server{
		name:   o.Name,
		addrs:  append([]netip.Addr(nil), o.Addrs...),
		port:   o.Port,
		tlsCfg: o.TLS,
		log:    o.Log,
		bound:  make(chan struct{}),
	}

	s.httpSrv = &http.Server{
		Handler:           o.Handler,
		ReadHeaderTimeout: o.Timeouts.ReadHeader.Duration(),
		ReadTimeout:       o.Timeouts.Read.Duration(),
		WriteTimeout:      o.Timeouts.Write.Duration(),
		IdleTimeout:       o.Timeouts.Idle.Duration(),
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(o.Log.Handler(), slog.LevelWarn),
	}

	return s, nil
}

// bindAddr formats one address for net.Listen: an invalid netip.Addr -
// netbind.Resolve's way of saying "every address" - becomes the wildcard
// ":port"; everything else goes through net.JoinHostPort so IPv6 addresses
// are bracketed correctly.
func bindAddr(a netip.Addr, port int) string {
	if !a.IsValid() {
		return ":" + strconv.Itoa(port)
	}
	return net.JoinHostPort(a.String(), strconv.Itoa(port))
}

// openListeners binds every configured address, wrapping each in a TLS
// listener when s.tlsCfg is set. A failure on any address closes every
// listener already opened and fails the whole call: this Server must never
// end up quietly serving on fewer addresses than configured.
func (s *Server) openListeners() ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(s.addrs))
	for _, a := range s.addrs {
		addr := bindAddr(a, s.port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, fmt.Errorf("%s: binding %s: %w", s.name, addr, err)
		}
		if s.tlsCfg != nil {
			ln = tls.NewListener(ln, s.tlsCfg)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

// Serve opens one listener per configured address and serves all of them
// concurrently until one of them returns an error that is not
// http.ErrServerClosed, or until ctx is done, in which case it shuts down
// gracefully and returns nil (or whatever error Shutdown produced).
//
// If any address fails to bind, Serve opens no listeners at all: a
// misconfigured or already-in-use address fails the whole server rather
// than silently serving on fewer addresses than configured.
//
// ctx is used only to trigger that graceful shutdown; it is deliberately
// not the request base context. A base context deriving from ctx would
// cancel every in-flight request's context at the instant ctx is done,
// which is the moment Shutdown starts, so a handler that honours its
// context would abort mid-response instead of being allowed to drain -
// contradicting "gracefully" above. Request contexts instead derive from
// context.Background(), so http.Server.Shutdown's own wait for active
// connections to finish is what actually bounds a graceful shutdown.
func (s *Server) Serve(ctx context.Context) error {
	s.httpSrv.BaseContext = func(net.Listener) context.Context { return context.Background() }

	listeners, err := s.openListeners()
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listeners = listeners
	s.mu.Unlock()
	close(s.bound)

	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(l net.Listener) { errCh <- s.httpSrv.Serve(l) }(ln)
	}

	stopWatching := make(chan struct{})
	defer close(stopWatching)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.httpSrv.Shutdown(context.Background())
		case <-stopWatching:
		}
	}()

	var firstErr error
	for range listeners {
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) && firstErr == nil {
			firstErr = err
			// Stop the remaining listeners promptly rather than leaving
			// them serving while Serve already has its answer.
			_ = s.httpSrv.Close()
		}
	}
	return firstErr
}

// Bound returns a channel that is closed once Serve has bound every
// configured address. From then on the kernel queues incoming connections,
// so a client can connect even before the accept loops start. If binding
// fails, the channel is never closed and Serve returns the error instead.
// Serve must be called at most once per Server.
func (s *Server) Bound() <-chan struct{} {
	return s.bound
}

// Shutdown gracefully shuts down the underlying http.Server - which itself
// closes every listener passed to Serve - and additionally closes any
// listener this Server still has a reference to, in case Shutdown is
// called before Serve finished opening all of them.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpSrv.Shutdown(ctx)

	s.mu.Lock()
	listeners := s.listeners
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}

	return err
}
