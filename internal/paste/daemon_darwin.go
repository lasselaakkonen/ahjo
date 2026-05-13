//go:build darwin

// Package paste hosts the macOS-side daemon that bridges the host
// NSPasteboard into Incus containers so Ctrl+V image paste inside
// `ahjo claude` works the same way it does on the bare macOS host.
//
// The daemon listens on 127.0.0.1:18340 and is reached from inside a
// container via an Incus proxy device (listen=container:127.0.0.1:18340 ->
// connect=host.lima.internal:18340). A pair of shell shims at
// /usr/local/bin/{xclip,wl-paste} inside the container call this endpoint
// when Claude's paste path probes for image data — see internal/incus/paste.go.
package paste

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// statusRecorder snags the response status so logged() can print it after
// the handler returns. http.ResponseWriter doesn't expose the status, and
// instrumenting every handler manually would be tedious.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func logged(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		h(sr, r)
		log.Printf("%s %s %s -> %d (%d bytes, %s)",
			r.Method, name, r.RemoteAddr, sr.status, sr.bytes, time.Since(start).Round(time.Millisecond))
	}
}

// ListenAddr is the loopback endpoint the paste daemon binds. Picked one
// off cc-clip's 18339 so both tools can coexist on the same host.
const ListenAddr = "127.0.0.1:18340"

// Run is the entry point for the hidden `ahjo paste-daemon` subcommand.
// Blocks serving HTTP until SIGTERM (sent by launchd on bootout). launchd's
// KeepAlive=true restarts the process if it exits non-cleanly, so a stray
// crash is self-healing.
func Run() error {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("paste-daemon starting on %s", ListenAddr)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", logged("healthz", handleHealth))
	mux.HandleFunc("/image.png", logged("image.png", handleImage))

	ln, err := net.Listen("tcp", ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ListenAddr, err)
	}
	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(idle)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	<-idle
	return nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleImage drives the JXA reader, then streams the PNG back. Anything
// short of "real PNG bytes on disk" surfaces as 204 No Content so the
// in-container shim can map it to xclip's "no selection" exit code without
// needing to inspect a body.
func handleImage(w http.ResponseWriter, _ *http.Request) {
	tmp, err := os.CreateTemp("", "ahjo-clip-*.png")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)

	if err := dumpPasteboardPNG(path); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	_, _ = w.Write(data)
}

// jxaReadPNG is JavaScript-for-Automation that pulls the front clipboard
// image and atomically writes it to argv[0] as PNG. Prefers the
// already-PNG representation; falls back to TIFF→PNG via NSBitmapImageRep
// so screenshots taken with Cmd+Shift+Ctrl+4 (which only land as TIFF on
// older macOS versions) still work.
//
// `osascript -l JavaScript` ships with stock macOS — no extra deps, no
// cgo, survives the CGO_ENABLED=0 dist build.
const jxaReadPNG = `
ObjC.import('AppKit');
function run(argv) {
  var out = argv[0];
  var pb = $.NSPasteboard.generalPasteboard;
  var data = pb.dataForType('public.png');
  if (!data || data.isNil()) {
    var tiff = pb.dataForType('public.tiff');
    if (tiff && !tiff.isNil()) {
      var rep = $.NSBitmapImageRep.imageRepWithData(tiff);
      if (rep && !rep.isNil()) {
        data = rep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $());
      }
    }
  }
  if (!data || data.isNil()) { $.exit(1); }
  data.writeToFileAtomically(out, true);
}
`

func dumpPasteboardPNG(outPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", jxaReadPNG, outPath)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
