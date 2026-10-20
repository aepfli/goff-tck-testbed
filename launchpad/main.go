package main

// launchpad -- the provider-TCK control API for a GO Feature Flag relay proxy.
//
// It runs as PID 1, serves the TCK control API (Appendix F,
// specification/assets/provider-tck/openapi/control-api.yaml) and supervises the relay proxy.
//
// The evaluation engine under test is real: the relay proxy is GOFF's own binary, copied unmodified
// from the vendor image, evaluating with GOFF's own core. Only the *source* of its configuration is
// synthetic -- a flag file the launchpad generates from the canonical set and points GOFF's file
// retriever at. Fake the config source, never the evaluation.

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// Fixed admin key. The control API has no way to communicate connection parameters, and the
	// launchpad is the only client that needs this one. Providers connect without a key: no
	// evaluation keys are configured.
	adminKey = "provider-tck-admin-key"

	changingFlag = "changing-flag"
)

type launchpad struct {
	// opMu serialises control operations. The TCK drives them one at a time anyway; holding it for
	// the whole operation means a /change can never interleave with a /start's regeneration.
	opMu sync.Mutex

	config   string // configuration currently in effect
	baseline flagSet
	current  flagSet

	proxy         *relayProxy
	canonicalPath string
	configDir     string
}

func main() {
	var (
		controlPort   = flag.Int("port", 8080, "control API port")
		proxyPort     = flag.Int("proxy-port", 1031, "relay proxy port")
		canonicalPath = flag.String("flags", "/flags/canonical-flags.json", "canonical flag set")
		configDir     = flag.String("configs", "/configs", "named configuration directory")
		workDir       = flag.String("work", "/goff", "directory for the generated proxy config and flag file")
		healthcheck   = flag.Bool("healthcheck", false, "probe the control API's /healthz and exit (for the compose healthcheck)")
	)
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*controlPort))
	}

	lp := &launchpad{
		proxy:         newRelayProxy(*workDir+"/goff-proxy.yaml", *workDir+"/flags.json", *proxyPort, adminKey),
		canonicalPath: *canonicalPath,
		configDir:     *configDir,
	}
	if err := lp.proxy.writeConfig(); err != nil {
		log.Fatalf("writing relay proxy config: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/start", lp.handleStart)
	mux.HandleFunc("/stop", lp.handleStop)
	mux.HandleFunc("/restart", lp.handleRestart)
	mux.HandleFunc("/change", lp.handleChange)
	mux.HandleFunc("/reset", lp.handleReset)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *controlPort), Handler: logRequests(mux)}
	go func() {
		log.Printf("launchpad listening on :%d (relay proxy on :%d)", *controlPort, *proxyPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("control API: %v", err)
		}
	}()

	// Boot into the default configuration so the container is usable without an explicit /start.
	lp.opMu.Lock()
	if err := lp.start("default"); err != nil {
		log.Printf("initial start failed: %v", err)
	}
	lp.opMu.Unlock()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = lp.proxy.stop()
}

func runHealthcheck(port int) int {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			q := ""
			if r.URL.RawQuery != "" {
				q = "?" + r.URL.RawQuery
			}
			log.Printf("%s %s%s", r.Method, r.URL.Path, q)
		}
		next.ServeHTTP(w, r)
	})
}

// start regenerates the named configuration's baseline, (re)starts the relay proxy on it, and
// returns only once that baseline is actually being served.
//
// It always restarts the process, even when it is already running: /start is the control API's
// "start the backend" operation, and a TCK that falls back to it for isolation should get the
// same process lifecycle every time. /reset is the no-blip alternative.
func (lp *launchpad) start(config string) error {
	path := lp.canonicalPath
	if config != "default" {
		candidate := fmt.Sprintf("%s/%s.json", lp.configDir, config)
		if _, err := os.Stat(candidate); err != nil {
			return errUnknownConfig{config}
		}
		path = candidate
	}
	set, err := buildFlagSet(path)
	if err != nil {
		return err
	}
	want, ok := set.defaultValue(changingFlag)
	if !ok {
		return fmt.Errorf("configuration %q has no %s to probe", config, changingFlag)
	}

	if err := lp.proxy.stop(); err != nil {
		return err
	}
	if err := set.write(lp.proxy.flagsPath); err != nil {
		return err
	}
	lp.config, lp.baseline, lp.current = config, set, set.clone()

	if err := lp.proxy.start(); err != nil {
		return err
	}
	return lp.proxy.probeServing(changingFlag, want, probeTimeout)
}

type errUnknownConfig struct{ name string }

func (e errUnknownConfig) Error() string { return fmt.Sprintf("unknown configuration %q", e.name) }

func (lp *launchpad) handleStart(w http.ResponseWriter, r *http.Request) {
	config := r.URL.Query().Get("config")
	if config == "" {
		config = "default"
	}
	lp.opMu.Lock()
	defer lp.opMu.Unlock()
	if err := lp.start(config); err != nil {
		code := http.StatusInternalServerError
		if _, ok := err.(errUnknownConfig); ok {
			code = http.StatusBadRequest
		}
		writeError(w, code, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (lp *launchpad) handleStop(w http.ResponseWriter, r *http.Request) {
	lp.opMu.Lock()
	defer lp.opMu.Unlock()
	if err := lp.proxy.stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleRestart kills the relay proxy, waits, and starts it again on the flag file as it stands --
// /change mutations included, because /restart preserves flag state.
func (lp *launchpad) handleRestart(w http.ResponseWriter, r *http.Request) {
	seconds := 5
	if s := r.URL.Query().Get("seconds"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("seconds: want a non-negative integer, got %q", s))
			return
		}
		seconds = n
	}
	lp.opMu.Lock()
	defer lp.opMu.Unlock()
	if lp.current == nil {
		writeError(w, http.StatusConflict, fmt.Errorf("not started"))
		return
	}
	if err := lp.proxy.stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	time.Sleep(time.Duration(seconds) * time.Second)
	if err := lp.proxy.start(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	want, _ := lp.current.defaultValue(changingFlag)
	if err := lp.proxy.probeServing(changingFlag, want, probeTimeout); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleChange toggles changing-flag's default variation between foo and bar and returns only once
// the new value is served.
//
// Write file -> admin refresh -> probe. No fsnotify, no sleep: the refresh is synchronous, and the
// probe proves it. And because /start and /reset regenerate from the canonical source rather than
// from the file this rewrites, a toggle cannot leak across them (flagd-testbed's /change does).
//
// While the relay proxy is stopped the toggle is still applied to the flag file and the call
// returns 200 without probing: nothing can be served during an outage, and the change becomes
// visible on the next /restart. That is deliberate -- it is the only way to drive "configuration
// changed during an outage" (FINDINGS.md #12). A /start would discard it, as it must.
func (lp *launchpad) handleChange(w http.ResponseWriter, r *http.Request) {
	lp.opMu.Lock()
	defer lp.opMu.Unlock()
	if lp.current == nil {
		writeError(w, http.StatusConflict, fmt.Errorf("not started"))
		return
	}
	f, ok := lp.current[changingFlag]
	if !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("configuration %q has no %s", lp.config, changingFlag))
		return
	}
	next := "bar"
	if f.DefaultRule.Variation == "bar" {
		next = "foo"
	}
	if _, ok := f.Variations[next]; !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("%s has no variation %q to toggle to", changingFlag, next))
		return
	}
	f.DefaultRule.Variation = next
	lp.apply(w)
}

// handleReset restores the current configuration's baseline without restarting the relay proxy:
// write file -> admin refresh -> probe. No availability blip, which matters more here than for
// flagd -- both hops of a GOFF provider poll, so a /start outage is expensive and hard for a
// provider to tell apart from a real event.
func (lp *launchpad) handleReset(w http.ResponseWriter, r *http.Request) {
	lp.opMu.Lock()
	defer lp.opMu.Unlock()
	if lp.baseline == nil {
		writeError(w, http.StatusConflict, fmt.Errorf("not started"))
		return
	}
	lp.current = lp.baseline.clone()
	lp.apply(w)
}

// apply publishes lp.current: writes the flag file and, if the relay proxy is up, refreshes it and
// waits until the new changing-flag value is served.
func (lp *launchpad) apply(w http.ResponseWriter) {
	if err := lp.current.write(lp.proxy.flagsPath); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !lp.proxy.running() {
		log.Printf("relay proxy is stopped: flag file updated, will be served on /restart")
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := lp.proxy.refresh(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	want, _ := lp.current.defaultValue(changingFlag)
	if err := lp.proxy.probeServing(changingFlag, want, probeTimeout); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeError(w http.ResponseWriter, code int, err error) {
	log.Printf("error: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}
