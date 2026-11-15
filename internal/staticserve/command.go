package staticserve

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// CommandName is the subcommand both binaries answer to.
const CommandName = "serve-static"

// shutdownGrace bounds how long a stopping server waits for in-flight
// responses. A static app's longest response is a large download, so this is
// generous by the standards of a file server and still well inside what either
// supervisor allows before SIGKILL.
const shutdownGrace = 15 * time.Second

// Serve is the `serve-static` subcommand: the workload a static app runs.
//
// It takes the release directory and an address, and nothing else. Root, entry
// and fallback are *resolved from the release* rather than passed in, which is
// the whole of T4a's argument applied to all three keys instead of only to the
// entry: a value that travels separately from the tree it describes is a value
// that can disagree with it — at a wake, at a boot, after a rollback — and the
// declaration is a file in the tree, so resolving it here means every start of
// every release in either execution mode reaches the same answer with no
// precedence rule to get wrong.
func Serve(args []string) error {
	fs := flag.NewFlagSet(CommandName, flag.ContinueOnError)
	dir := fs.String("dir", "", "the release directory to serve from")
	port := fs.Int("port", 0, "TCP port to listen on")
	host := fs.String("host", "127.0.0.1", "address to bind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *port == 0 {
		return errors.New("serve-static needs --dir and --port; it is started by Toft, not by hand")
	}

	cfg, err := Resolve(*dir)
	if err != nil {
		return err
	}
	handler, err := NewHandler(cfg)
	if err != nil {
		return fmt.Errorf("opening the document root: %w", err)
	}
	defer handler.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One line at startup and nothing per request. A file server has nothing to
	// say about a GET, and the two obvious ways to say it anyway are both bad:
	// a row per request is a DB write on the proxy path, and a stdout line per
	// asset floods the log store the app's *build* output lives in.
	srv := &http.Server{
		Addr:              net.JoinHostPort(*host, strconv.Itoa(*port)),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Printf("serving %d file(s) from %s, entry %s%s, on %s",
		cfg.Files, describeRoot(cfg.RootRel), cfg.Entry, fallbackNote(cfg.Fallback), srv.Addr)

	done := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func fallbackNote(on bool) string {
	if on {
		return " (SPA fallback on)"
	}
	return ""
}
