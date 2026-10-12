// Command hwi-api serves a Linux host's hardware and OS inventory over
// HTTP.
//
// This file is the composition root and nothing else: it parses flags,
// loads configuration, and wires the packages below internal/ together.
// The only policy it owns is the host-identity derivation in hostID, which
// has no other natural home because it exists precisely to keep a value
// the rest of the agent never sees out of every payload.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/access"
	"github.com/krisiasty/hwi-api/internal/api"
	"github.com/krisiasty/hwi-api/internal/auth"
	"github.com/krisiasty/hwi-api/internal/buildinfo"
	"github.com/krisiasty/hwi-api/internal/config"
	"github.com/krisiasty/hwi-api/internal/helper"
	"github.com/krisiasty/hwi-api/internal/inventory"
	"github.com/krisiasty/hwi-api/internal/logging"
	"github.com/krisiasty/hwi-api/internal/metrics"
	"github.com/krisiasty/hwi-api/internal/netbind"
	"github.com/krisiasty/hwi-api/internal/ops"
	"github.com/krisiasty/hwi-api/internal/pciids"
	"github.com/krisiasty/hwi-api/internal/server"
)

// builtinConfigPath is where the packaged unit puts the configuration file.
const builtinConfigPath = "/etc/hwi-api/config.yaml"

// configEnv overrides builtinConfigPath without a command line flag, which
// is what a container image or a drop-in unit file can set.
const configEnv = "HWI_API_CONFIG"

// helperSocketEnv names the Unix socket of the privileged helper (hwi-api
// helper, internal/helper), which reads the root-only DMI files, the SMBIOS
// table and PCI Vital Product Data on the daemon's behalf. The packaged
// unit sets it. Unset, the daemon reads only what its own user may.
const helperSocketEnv = "HWI_API_HELPER_SOCKET"

// shutdownDrain is how long both listeners keep serving after readiness has
// flipped to not-ready. Without it the listeners would close in the same
// instant /readyz started failing, so no load balancer or supervisor would
// ever observe the transition and take the agent out of rotation before its
// connections were cut.
const shutdownDrain = time.Second

// shutdownTimeout bounds the graceful shutdown of both listeners.
// config.TimeoutConfig has no shutdown field to read this from, so without a
// deadline supplied here a single held connection would block process exit
// indefinitely.
const shutdownTimeout = 10 * time.Second

// opsTimeouts bounds request handling on the ops listener.
//
// These are fixed here rather than configurable because config.OpsConfig is
// deliberately a two-field surface (bind and access), and there is nothing
// here an operator would need to tune: the ops listener serves a closed,
// known set of four fast endpoints, none of which reads a request body or
// writes a large response. Leaving them at zero - which is what an
// unconfigured config.TimeoutConfig gives - is not an option: a net/http
// server with no timeouts lets a single slow or idle peer hold a connection
// indefinitely, and on this listener that would also make shutdownTimeout
// the only reason the process ever exits.
//
// They are deliberately not read from cfg.API.Timeouts either: that
// coupling would mean an operator tuning the API listener silently retuned
// this one. The values mirror the API defaults except for Write, which can
// be shorter because the slowest response here is a metrics render, not a
// full inventory snapshot.
var opsTimeouts = config.TimeoutConfig{
	ReadHeader: config.Duration(5 * time.Second),
	Read:       config.Duration(10 * time.Second),
	Write:      config.Duration(15 * time.Second),
	Idle:       config.Duration(60 * time.Second),
}

// opsMaxConcurrentRequests caps in-flight requests on the ops listener,
// except /healthz; see opsHandler. Like opsTimeouts it is fixed rather
// than configurable, so that the unauthenticated listener's bound cannot
// be loosened through the file. Its expected clients are a handful of
// local ones - a Prometheus scrape, a health checker, an operator - so 16
// is far above normal load while still bounding how many /metrics renders
// can run at once.
const opsMaxConcurrentRequests = 16

// machineIDPaths are the two standard locations for the host's machine ID,
// tried in the order systemd itself documents. The paths are relative
// because hostID reads them through an fs.FS rooted at "/".
var machineIDPaths = []string{"etc/machine-id", "var/lib/dbus/machine-id"}

// appUUID namespaces the host identifier hostID derives. systemd documents
// the raw machine ID as confidential - it is a host-unique value usable to
// correlate a machine across unrelated services - so the agent never
// publishes it. Keying an HMAC with this fixed, application-specific UUID
// means the published identifier cannot be reversed to the machine ID, and
// cannot be matched against any other application's derivation of the same
// machine ID. It is therefore part of the wire contract: changing it
// changes every host's reported identity, so it must never be edited.
var appUUID = []byte{
	0x2f, 0x1c, 0x9a, 0x6d, 0x4b, 0x83, 0x47, 0xe2,
	0x9d, 0x50, 0x6a, 0x71, 0xc8, 0x3e, 0xb4, 0x15,
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "hwi-api: %v\n", err)
		os.Exit(1)
	}
}

// run starts the agent and returns when ctx is cancelled or a listener
// fails. A probe failure is deliberately not one of the ways it can fail:
// hardware probes fail routinely in virtual machines and containers, and
// the envelope's complete/disabled/errors contract exists so that a partial
// inventory is served rather than withheld.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	// `hwi-api helper` is the privileged helper, which systemd starts per
	// connection (packaging/hwi-api-helper@.service). It shares the
	// binary so that daemon and helper always come from one release.
	if len(args) > 0 && args[0] == "helper" {
		return helper.Main(ctx, args[1:], stderr)
	}

	started := time.Now()
	build := buildinfo.Get()

	flags := flag.NewFlagSet(build.Name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to the configuration file")
	showVersion := flags.Bool("version", false, "print build information and exit")
	if err := flags.Parse(args); err != nil {
		// -h is a successful request for usage, which Parse has already
		// written to stderr, not a failed start.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		if _, err := fmt.Fprintf(stderr, "%s %s\ncommit: %s\nbuilt: %s\ngo: %s\n",
			build.Name, build.Version, build.Commit, build.Date, build.GoVersion); err != nil {
			return fmt.Errorf("writing version information: %w", err)
		}
		return nil
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: hwi-api takes options only, or the helper subcommand", flags.Arg(0))
	}

	cfg, warnings, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := logging.New(cfg.Log.Level, stderr)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		logger.Warn("configuration warning", "detail", w)
	}
	logger.Info("starting",
		"version", build.Version, "commit", build.Commit, "config", *configPath)

	// rootFS is the host's real root. Every filesystem-reading collector
	// takes it as a parameter so each is testable against an fstest.MapFS,
	// which is why the paths they read are relative.
	rootFS := os.DirFS("/")

	enabled := inventory.EnabledSet(cfg.Inventory.Refresh)
	store := inventory.NewStore(enabled)
	agentMetrics := metrics.New(build, store, rootFS, time.Now)
	schedules := inventory.BuildSchedules(cfg.Inventory.Refresh)
	refresher := inventory.NewRefresher(store, newProbers(store, rootFS, logger), schedules,
		logger, agentMetrics)

	// ready is true once the first collection has attempted every enabled
	// subsystem and the API listener is bound, until shutdown begins.
	// apiBound is set when the API listener has bound (see below), and
	// draining at the start of shutdown so that readiness reports not-ready
	// while in-flight requests finish. Both the ops /readyz and /status
	// handlers and the hwi_api_ready gauge are driven from this one
	// predicate, so they can never disagree.
	var apiBound, draining atomic.Bool
	ready := func() bool { return apiBound.Load() && !draining.Load() && store.Ready() }

	apiAddrs, err := netbind.Resolve(cfg.API.Bind.Addresses, cfg.API.Bind.Interfaces, netbind.System{})
	if err != nil {
		return fmt.Errorf("api.bind: %w", err)
	}
	opsAddrs, err := netbind.Resolve(cfg.Ops.Bind.Addresses, cfg.Ops.Bind.Interfaces, netbind.System{})
	if err != nil {
		return fmt.Errorf("ops.bind: %w", err)
	}
	apiAccess, err := access.New(cfg.API.Access.Allow, cfg.API.Access.Deny)
	if err != nil {
		return fmt.Errorf("api.access: %w", err)
	}
	opsAccess, err := access.New(cfg.Ops.Access.Allow, cfg.Ops.Access.Deny)
	if err != nil {
		return fmt.Errorf("ops.access: %w", err)
	}

	// THE invariant of this file, whenever both TLS and auth are enabled:
	// exactly one *auth.Authenticator is constructed, and that same value is
	// passed both to server.BuildTLSConfig - which takes its client CA pool
	// and sets the ClientAuth mode that makes crypto/tls populate
	// ConnectionState.VerifiedChains at all - and to server.Authenticate,
	// whose mTLS path trusts exactly those VerifiedChains. Building a
	// second authenticator for either would produce a listener whose TLS
	// layer verifies against one pool while its auth layer believes
	// another, and nothing anywhere would report an error.
	//
	// Both halves of that pair are now independently optional, gated on
	// cfg.API.TLS.Enabled and cfg.API.Auth.Enabled respectively - not
	// inferred from which mechanisms happen to be set, which is what would
	// let an absent or mistyped api.auth block silently mean "open" again.
	// This authenticator is nil whenever cfg.API.Auth.Enabled is false: an
	// authenticator built from an all-disabled config would still reject
	// every request with auth.ErrNoCredentials, so the alternative to
	// omitting it here would be a listener that claims to have no
	// authentication configured yet refuses every caller anyway. When
	// Enabled is true, config.Validate has already guaranteed at least one
	// of MTLS.Enabled/Token.Enabled is also true, so auth.New never sees
	// the all-disabled case. tlsCfg is likewise nil whenever TLS is
	// disabled, which server.Options.TLS already documents as its
	// plaintext-listener case. The two are independent: a nil authenticator
	// is still passed to BuildTLSConfig when TLS is enabled (its documented
	// no-mTLS case), and a non-nil authenticator still governs
	// server.Authenticate when TLS is disabled.
	var authenticator *auth.Authenticator
	if cfg.API.Auth.Enabled {
		// Without TLS at this listener no request can carry a client
		// certificate, so mTLS is inert; config.Validate has already
		// warned, and guaranteed token auth is on in that case. Switching
		// it off here keeps auth.New from loading a CA file that
		// validation deliberately did not require.
		authCfg := cfg.API.Auth
		if !cfg.API.TLS.Enabled {
			authCfg.MTLS.Enabled = false
		}
		authenticator, err = auth.New(authCfg)
		if err != nil {
			return fmt.Errorf("api.auth: %w", err)
		}
	}
	var tlsCfg *tls.Config
	if cfg.API.TLS.Enabled {
		tlsCfg, err = server.BuildTLSConfig(cfg.API.TLS, authenticator)
		if err != nil {
			return fmt.Errorf("api.tls: %w", err)
		}
	}

	apiMux := http.NewServeMux()
	api.New(store, hostID(rootFS), func(reqCtx context.Context, sub apiv1.Subsystem) {
		refresher.RefreshOne(reqCtx, sub, time.Now())
	}).Routes(apiMux)

	opsMux := http.NewServeMux()
	ops.New(agentMetrics, build, ready, started).Routes(opsMux)

	// server.Chain's first variadic argument is the outermost, so this slice
	// reads in the order requests actually flow through it. That order is
	// fixed and load-bearing: Recover must be able to catch a panic raised
	// inside any other middleware, and Observe must be above access control
	// so that rejected traffic is still counted and logged. Authenticate is
	// included only when authenticator is non-nil, i.e. only when
	// cfg.API.Auth.Enabled - with authentication disabled there is nothing
	// for it to check, and including it anyway would reject every request
	// with auth.ErrNoCredentials instead of the deliberate no-authentication
	// posture api.auth.enabled: false configured.
	apiMiddleware := []server.Middleware{
		server.Recover(logger),
		server.Observe("api", agentMetrics, logger),
		server.AccessControl("api", apiAccess, agentMetrics),
	}
	if authenticator != nil {
		apiMiddleware = append(apiMiddleware, server.Authenticate("api", authenticator, agentMetrics))
	}
	apiMiddleware = append(apiMiddleware,
		server.LimitConcurrency("api", cfg.API.MaxConcurrentRequests, agentMetrics))

	apiSrv, err := server.New(server.Options{
		Name:     "api",
		Addrs:    apiAddrs,
		Port:     cfg.API.Bind.Port,
		TLS:      tlsCfg,
		Timeouts: cfg.API.Timeouts,
		Handler:  server.Chain(apiMux, apiMiddleware...),
		// Tagged, because this logger also backs the http.Server's own
		// ErrorLog: a TLS handshake failure would otherwise be
		// indistinguishable from one on the other listener.
		Log: logger.With("listener", "api"),
	})
	if err != nil {
		return err
	}

	// config.OpsConfig carries only Bind and Access - no TLS, timeouts or
	// concurrency cap - so TLS stays nil, and timeouts and the cap come from
	// the fixed opsTimeouts and opsMaxConcurrentRequests above; see
	// opsHandler for the middleware.
	opsSrv, err := server.New(server.Options{
		Name:     "ops",
		Addrs:    opsAddrs,
		Port:     cfg.Ops.Bind.Port,
		Timeouts: opsTimeouts,
		Handler:  opsHandler(opsMux, logger, agentMetrics, opsAccess),
		Log:      logger.With("listener", "ops"),
	})
	if err != nil {
		return err
	}

	// background owns every goroutine run starts, so that run cannot return
	// while one is still live: the initial collection, the refresh loop and
	// both Serve calls. The loop and the Serve calls take contexts run
	// cancels on the way out; the initial collection takes ctx and stops
	// between probes once it is cancelled.
	var background sync.WaitGroup
	loopCtx, stopLoop := context.WithCancel(context.Background())
	defer func() {
		stopLoop()
		background.Wait()
	}()

	// serveCtx is separate from ctx so that shutdown is driven from below,
	// with the explicit deadline shutdownTimeout supplies; Serve's own
	// reaction to a cancelled context calls Shutdown with no deadline.
	serveCtx, stopServing := context.WithCancel(context.Background())
	defer stopServing()

	type listener struct {
		name string
		srv  *server.Server
	}
	type listenerExit struct {
		name string
		err  error
	}
	// Buffered for every listener, so a Serve goroutine can always deliver
	// its result and exit even when run has stopped reading.
	exits := make(chan listenerExit, 2)
	// serving lists the listeners started so far, which are the ones
	// shutdown below must stop.
	var serving []listener
	serve := func(l listener) {
		serving = append(serving, l)
		background.Go(func() {
			exits <- listenerExit{l.name, l.srv.Serve(serveCtx)}
		})
	}
	// listenerFailed reports a listener that returned on its own: a bind
	// failure, or a fatal Serve error. Whichever listener is still up is
	// shut down below before run returns, so neither a goroutine nor a
	// bound port outlives this function.
	listenerFailed := func(e listenerExit) error {
		if e.err != nil {
			logger.Error("listener stopped", "listener", e.name, "error", e.err)
			return fmt.Errorf("%s listener: %w", e.name, e.err)
		}
		logger.Error("listener stopped without being asked to", "listener", e.name)
		return fmt.Errorf("%s listener stopped without being asked to", e.name)
	}

	// The ops listener opens before the first collection, so a supervisor
	// can see the process is alive (/healthz) while a slow first
	// collection is still running, instead of finding the port closed and
	// restarting it. /readyz and hwi_api_ready stay not-ready until the
	// collection has finished and the API listener has bound; see ready.
	// The API listener still opens only after the first collection, so a
	// request it answers is never racing the startup probes.
	serve(listener{"ops", opsSrv})
	// "starting", not "listening": Serve binds asynchronously, so a bind
	// failure is still possible at this point and is reported below.
	logger.Info("starting ops listener", "ops_port", cfg.Ops.Bind.Port)

	// The first full collection runs in the background so that a shutdown
	// request or an ops listener failure is noticed without waiting for it.
	// It gets its own context, cancelled as soon as startup is abandoned,
	// so that after a listener failure RefreshAll stops at the next
	// subsystem instead of probing every remaining one before run can
	// return; only a probe already in flight is waited for, because the
	// probers do not watch their context. RefreshAll returns no error: a
	// failed probe is recorded in the store and startup continues.
	collectCtx, cancelCollect := context.WithCancel(ctx)
	defer cancelCollect()
	collected := make(chan struct{})
	background.Go(func() {
		defer close(collected)
		refresher.RefreshAll(collectCtx, time.Now())
	})

	var serveErr error
	// await blocks until next is closed, a shutdown is requested, or a
	// listener fails, and reports whether startup should carry on.
	await := func(next <-chan struct{}) bool {
		select {
		case <-next:
		case <-ctx.Done():
		case e := <-exits:
			serveErr = listenerFailed(e)
		}
		return serveErr == nil && ctx.Err() == nil
	}

	if !await(collected) {
		cancelCollect()
	} else {
		// Start must run after RefreshAll, never before: it adds each
		// subsystem's stagger offset to the next due time RefreshAll's own
		// refresh of it set, an interval after that probe finished, and
		// RefreshAll running afterwards would overwrite those offsets.
		refresher.Start(time.Now())
		background.Go(func() {
			refresher.Run(loopCtx)
		})

		beforeAPIServe()
		serve(listener{"api", apiSrv})
		// "starting", not "listening": Serve binds asynchronously, so a
		// bind failure is still possible at this point.
		logger.Info("starting api listener", "api_port", cfg.API.Bind.Port)

		if await(apiSrv.Bound()) {
			// Readiness waits for the API listener to be bound as well as
			// for the collection: store.Ready alone can turn true before
			// RefreshAll returns, or from the start when every subsystem is
			// disabled, and a supervisor must not route clients to a port
			// that is not accepting yet.
			apiBound.Store(true)
			// The gauge only ever goes false->true here and back at
			// shutdown, so setting it at those two points covers every
			// transition. It is set eagerly rather than from a registry
			// collector because collectors run after the families are
			// rendered, so a value only set inside one would always be a
			// scrape late.
			if ready() {
				agentMetrics.SetReady(true)
			}
			logger.Info("api listener bound", "ready", ready())

			select {
			case <-ctx.Done():
				logger.Info("shutdown requested")
			case e := <-exits:
				serveErr = listenerFailed(e)
			}
		}
	}
	if serveErr == nil && !apiBound.Load() {
		logger.Info("shutdown requested during startup")
	}

	// Readiness flips before anything closes, so a supervisor polling
	// /readyz sees the agent leave rotation while it can still serve.
	draining.Store(true)
	agentMetrics.SetReady(false)
	if serveErr == nil {
		// Only a requested shutdown has anything to drain; after a listener
		// failure, waiting would just delay the exit.
		time.Sleep(shutdownDrain)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	// Both listeners shut down concurrently, sharing one deadline: done in
	// sequence, a slow api drain would leave ops no time at all.
	shutdownErrs := make([]error, len(serving))
	var shutdownWG sync.WaitGroup
	for i, l := range serving {
		shutdownWG.Go(func() {
			if err := l.srv.Shutdown(shutdownCtx); err != nil {
				shutdownErrs[i] = fmt.Errorf("shutting down the %s listener: %w", l.name, err)
			}
		})
	}
	shutdownWG.Wait()

	logger.Info("stopped", "uptime_seconds", time.Since(started).Seconds())
	return errors.Join(append([]error{serveErr}, shutdownErrs...)...)
}

// opsHandler wraps the ops mux in its fixed middleware chain, outermost
// first, as for the API listener.
//
// The ops listener is plaintext and unauthenticated on loopback by design,
// so it gets no Authenticate: there are no credentials on it to check. It
// does get AccessControl: ops.access is a real setting, validated by
// config.Validate and defaulting to loopback only, so leaving that
// middleware off would silently serve /metrics and /status to every peer
// that can route to the port.
//
// Below access control, every request except /healthz goes through
// LimitConcurrency at the fixed opsMaxConcurrentRequests, so that an
// allowed peer cannot run unbounded concurrent work such as /metrics
// renders. /healthz bypasses it: liveness must stay 200 while the process
// is serving, and a limiter full of /metrics requests answering it with
// 503 would let a supervisor restart a healthy agent under exactly the
// load the limiter exists to contain. Its handler writes a small fixed
// response, and it still passes Recover, Observe, AccessControl and the
// server's own timeouts. The match is on the exact path Routes registers,
// before the mux runs; anything else, such as /healthz/, stays limited.
// Both branches forward r unchanged, as Observe requires.
func opsHandler(mux http.Handler, logger *slog.Logger, m *metrics.Agent, allow *access.Matcher) http.Handler {
	limited := server.Chain(mux, server.LimitConcurrency("ops", opsMaxConcurrentRequests, m))
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ops.HealthzPath {
			mux.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})
	return server.Chain(route,
		server.Recover(logger),
		server.Observe("ops", m, logger),
		server.AccessControl("ops", allow, m),
	)
}

// defaultConfigPath resolves the configuration path used when --config is
// not given.
func defaultConfigPath() string {
	if p := os.Getenv(configEnv); p != "" {
		return p
	}
	return builtinConfigPath
}

// newProbers is buildProbers, as a variable so that a test can substitute
// probers it controls, such as one that blocks to hold startup inside the
// initial collection.
var newProbers = buildProbers

// beforeAPIServe runs after the first collection and before the API
// listener starts. It does nothing; a test replaces it to hold startup in
// that window and check that readiness is still false there.
var beforeAPIServe = func() {}

// buildProbers assembles the complete prober set, each subsystem collected
// by the agent itself: product, baseboard, chassis, bios, block, cpu,
// links, memory, network, pci, slots, topology, the udev-* subsystems, os
// and virtualization.
func buildProbers(store *inventory.Store, rootFS fs.FS, logger *slog.Logger) inventory.Probers {
	// The privileged helper, when the unit configures one, as a read-only
	// file system: the root-only DMI files, the SMBIOS table and each PCI
	// device's VPD.
	var privileged fs.FS
	if socket := os.Getenv(helperSocketEnv); socket != "" {
		privileged = (&helper.Client{Socket: socket}).FS()
	}

	probers := inventory.Probers{}

	// The DMI subsystems read /sys/class/dmi/id. The daemon holds no
	// capabilities, so the root-only serial numbers and UUID come through
	// the helper; bios has none of those.
	probers[apiv1.SubsystemProduct] = inventory.NewProductProber(rootFS, privileged)
	probers[apiv1.SubsystemBaseboard] = inventory.NewBaseboardProber(rootFS, privileged)
	probers[apiv1.SubsystemChassis] = inventory.NewChassisProber(rootFS, privileged)
	probers[apiv1.SubsystemBIOS] = inventory.NewBIOSProber(rootFS)

	// block lists /sys/block itself, with the host's udev database for each
	// device's identity and the host's mount table, /proc/1/mountinfo, for
	// its mount points. Where /proc hides PID 1 it reads the agent's own
	// table, which also holds the unit's ExecPaths= bind of this binary
	// onto itself: a mount on no host, so block is told to leave it out.
	var selfBinds []string
	if exe, err := os.Executable(); err == nil {
		selfBinds = append(selfBinds, exe)
	}
	probers[apiv1.SubsystemBlock] = inventory.NewBlockProber(rootFS, selfBinds...)

	// cpu groups the logical CPUs of /sys/devices/system/cpu into packages
	// and cores, and names each package from /proc/cpuinfo.
	probers[apiv1.SubsystemCPU] = inventory.NewCPUProber(rootFS)
	// topology groups the same CPUs, with their caches and memory, by NUMA
	// node from /sys/devices/system/node.
	probers[apiv1.SubsystemTopology] = inventory.NewTopologyProber(rootFS)

	probers[apiv1.SubsystemLinks] = inventory.NewLinksProber(inventory.SystemLinks{RootFS: rootFS})
	// network lists the interfaces from sysfs and asks the kernel's ethtool
	// netlink family about each, so the host needs no ethtool binary. Each
	// interface's permanent address comes from one rtnetlink link dump.
	probers[apiv1.SubsystemNetwork] = inventory.NewNetworkProber(rootFS, inventory.DialEthtool, inventory.PermanentAddresses)

	// The memory modules come from the SMBIOS table, which is root-only
	// like the DMI identity fields above and read through the helper.
	probers[apiv1.SubsystemMemory] = inventory.NewMemoryProber(rootFS, privileged, logger)

	// The agent enumerates PCI devices from sysfs itself, naming them from
	// the PCI ID database bundled into it, never the host's. The bundled
	// copy is checked by the pciids tests, so it cannot fail to parse in a
	// released binary; if it somehow did, devices would still be listed,
	// with every name "unknown".
	pciDB, err := pciids.Bundled()
	if err != nil {
		logger.Error("the bundled PCI ID database is unreadable; PCI names will be unknown", "error", err)
	}
	probers[apiv1.SubsystemPCI] = inventory.NewPCIProber(rootFS, pciDB, privileged)
	// slots lists the expansion slots from the SMBIOS table, read through
	// the helper like the memory modules.
	probers[apiv1.SubsystemSlots] = inventory.NewSlotsProber(privileged, logger)

	// Each udev-* subsystem reports its own kinds of device from udev's
	// database, which the daemon can read as its own user.
	for sub, kinds := range inventory.UdevKinds {
		probers[sub] = inventory.NewUdevProber(rootFS, kinds)
	}

	probers[apiv1.SubsystemOS] = inventory.NewOSProber(rootFS, inventory.Uname, os.Hostname)

	// Virtualization detection reads signals the store already holds rather
	// than probing again. apiv1's canonical order puts virtualization last,
	// and both BuildSchedules and RefreshAll follow that order, so the cpu
	// and DMI entries it reads are already populated when this runs.
	probers[apiv1.SubsystemVirtualization] = inventory.NewVirtualizationProber(
		func(context.Context) (inventory.VirtSignals, error) {
			return collectVirtSignals(store, rootFS), nil
		})

	return probers
}

// dmiSources maps each DMI signal name inventory.Detect weighs to the
// stored payload and JSON field it is read from. It is a slice, not a map,
// so the assembled signals never depend on iteration order.
var dmiSources = []struct {
	signal string
	sub    apiv1.Subsystem
	field  string
}{
	{"sys_vendor", apiv1.SubsystemProduct, "vendor"},
	{"product_name", apiv1.SubsystemProduct, "name"},
	{"bios_vendor", apiv1.SubsystemBIOS, "vendor"},
	{"board_vendor", apiv1.SubsystemBaseboard, "vendor"},
	{"chassis_vendor", apiv1.SubsystemChassis, "vendor"},
}

// collectVirtSignals assembles what inventory.Detect needs from the store's
// current cpu, product, bios, baseboard and chassis entries plus the two
// kernel-exported hypervisor hints. It never fails: an absent signal is
// simply absent, which Detect already treats as "no evidence".
func collectVirtSignals(store *inventory.Store, rootFS fs.FS) inventory.VirtSignals {
	capabilities := cpuCapabilities(store)
	signals := inventory.VirtSignals{
		// CPUDataPresent separates "the CPU reported no hypervisor flag",
		// which is strong bare-metal evidence, from "no CPU flags were
		// available at all", which is none - so it is set only when the cpu
		// entry actually yielded capabilities, and only when the host's
		// cpuinfo lists them as x86 flags. arm64's Features never carry a
		// hypervisor bit, so a guest's Features look exactly like a
		// bare-metal host's.
		CPUDataPresent:       len(capabilities) > 0 && inventory.ReportsHypervisorFlag(rootFS),
		CPUCapabilities:      capabilities,
		DMI:                  make(map[string]string, len(dmiSources)),
		HypervisorType:       readHostFile(rootFS, "sys/hypervisor/type"),
		DeviceTreeCompatible: readHostFile(rootFS, "proc/device-tree/hypervisor/compatible"),
	}

	for _, src := range dmiSources {
		if v := storedString(store, src.sub, src.field); v != "" {
			signals.DMI[src.signal] = v
		}
	}
	return signals
}

// cpuCapabilities returns the union of every processor's capability list
// from the stored cpu payload, sorted so the same host always yields the
// same evidence.
func cpuCapabilities(store *inventory.Store) []string {
	body, ok := storedBody(store, apiv1.SubsystemCPU)
	if !ok {
		return nil
	}
	var payload struct {
		Processors []struct {
			Capabilities []string `json:"capabilities"`
		} `json:"processors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	for _, p := range payload.Processors {
		for _, c := range p.Capabilities {
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// storedBody returns a subsystem's stored body when it has one. An entry
// whose most recent probe failed keeps its previous body, which is the
// right input here: last-good DMI is far better evidence than none.
func storedBody(store *inventory.Store, sub apiv1.Subsystem) (json.RawMessage, bool) {
	e, ok := store.Get(sub)
	if !ok || len(e.Body) == 0 {
		return nil, false
	}
	return e.Body, true
}

// storedString reads one top-level string field out of a stored payload,
// returning "" for anything missing or of another type.
func storedString(store *inventory.Store, sub apiv1.Subsystem, field string) string {
	body, ok := storedBody(store, sub)
	if !ok {
		return ""
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	raw, ok := payload[field]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// readHostFile reads one small pseudo-file, returning "" when it is absent
// or unreadable - which is the normal case for most of them on most hosts.
// Device-tree properties are NUL-terminated, so NUL is trimmed alongside
// surrounding whitespace.
func readHostFile(fsys fs.FS, name string) string {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return ""
	}
	return strings.Trim(string(b), "\x00 \t\r\n")
}

// hostID derives the agent's published host identifier from the machine ID
// found in fsys, which is os.DirFS("/") in production - hence the relative
// paths in machineIDPaths. It returns "" when no machine ID exists, so that
// a host without one reports no identity rather than a made-up one.
//
// The machine ID itself is never published: see appUUID for why, and for
// why the derivation must not change. Whatever the file holds is hashed
// after trimming whitespace, with no format check - a machine ID is
// documented as hex but the agent has no business refusing to identify a
// host whose ID is merely unconventional.
func hostID(fsys fs.FS) string {
	raw := ""
	for _, p := range machineIDPaths {
		if v := readHostFile(fsys, p); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return ""
	}

	mac := hmac.New(sha256.New, appUUID)
	mac.Write([]byte(raw))
	// Truncated to 128 bits: enough to be collision-free across any
	// realistic number of hosts, and short enough to be a usable key.
	return hex.EncodeToString(mac.Sum(nil)[:16])
}
