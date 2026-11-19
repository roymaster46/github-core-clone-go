// Entry <-> env lifecycle for the github-core-clone Go forge.
//
// Port of the Node entry's main.ts: the trial env (the suite) drives
// everything through two endpoints, and this file is only the glue
// between the forge in app.go and that env:
//
//   1. The env's origin comes from MAKEFUN_ENV_URL. With no origin there
//      is nobody to talk to, so the process fails fast with exit code 1.
//   2. The forge binds an ephemeral port (listen on :0 — the kernel
//      chooses). Port 8080 is the env's own and is never bound here.
//   3. The bound port is announced with POST {origin}/register
//      {"port": N}. A 409 means some other entry registered first; that
//      is fine, this process simply carries on waiting like any other.
//   4. The process then polls GET {origin}/done until it reads
//      {"done": true}. While the suite is still running the env answers
//      {"done": false} at once, so this is a poll loop, never a
//      long-held request. A failed or slow poll is retried; the process
//      never exits before the env says the trial is over.
//   5. On {"done": true} the server is closed and the process exits 0.
//
// Nothing else lives here: just createHandler() from app.go and the two
// HTTP calls above.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// The env origin, without trailing slashes so URL building stays clean.
var envOrigin = strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

// One register or poll request may take this long before it is abandoned.
const requestBudget = 30 * time.Second

// Backoff between retries: start here, double each time, never pass the cap.
const backoffStart = 250 * time.Millisecond
const backoffCap = 2 * time.Second

var httpClient = &http.Client{Timeout: requestBudget}

func pause(d time.Duration) { time.Sleep(d) }

// backoffMs returns the wait before retry number n (0-based):
// exponential, capped at backoffCap.
func backoffFor(n int) time.Duration {
	d := backoffStart
	for i := 0; i < n && i < 8; i++ {
		d *= 2
	}
	if d > backoffCap {
		return backoffCap
	}
	return d
}

// keepTrying runs step until it reports success, retrying after every
// failure. step returns true when the work is done, false when it should
// be tried again, and an error only for a fatal error that must end the
// process.
func keepTrying(step func() (bool, error)) error {
	for attempt := 0; ; attempt++ {
		done, err := step()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		pause(backoffFor(attempt))
	}
}

// announcePort announces the bound port to the env. Succeeds on HTTP 200,
// and also on 409 (another entry registered first — not ours to fix).
// HTTP 400/405 are contract errors and abort the process; any other
// status, or a network failure, just means the env may still be
// starting, so it is retried. Response bodies are always drained before
// closing so the connection can be reused by the next retry.
func announcePort(port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	return keepTrying(func() (bool, error) {
		req, err := http.NewRequest("POST", envOrigin+"/register", bytes.NewReader(body))
		if err != nil {
			return false, err
		}
		req.Header.Set("content-type", "application/json")
		res, err := httpClient.Do(req)
		if err != nil {
			return false, nil
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode == 200 || res.StatusCode == 409 {
			return true, nil
		}
		if res.StatusCode == 400 || res.StatusCode == 405 {
			return false, fmt.Errorf("register answered HTTP %d", res.StatusCode)
		}
		return false, nil
	})
}

// awaitFinished polls GET /done until the env answers {"done": true};
// never returns sooner. The body is drained after decoding so the poll
// connection is reused instead of re-dialled every couple of seconds
// for the whole trial.
func awaitFinished() error {
	return keepTrying(func() (bool, error) {
		res, err := httpClient.Get(envOrigin + "/done")
		if err != nil {
			return false, nil
		}
		var parsed struct {
			Done bool `json:"done"`
		}
		decErr := json.NewDecoder(res.Body).Decode(&parsed)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			return false, nil
		}
		if decErr != nil {
			return false, nil
		}
		return parsed.Done, nil
	})
}

func run() error {
	if envOrigin == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	store := NewStore()
	server, err := listenTransport(createHandler(store))
	if err != nil {
		return err
	}
	addr, ok := server.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("the server did not bind an ephemeral TCP port")
	}
	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", addr.Port)

	// The trial ends with SIGTERM: close the server and exit 0 instead
	// of dying on the signal (a 143 exit reads as a crash to the harness).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		_ = server.Close()
		os.Exit(0)
	}()

	if err := announcePort(addr.Port); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	if err := awaitFinished(); err != nil {
		return err
	}
	_ = server.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
}
