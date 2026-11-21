package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/internal/kmap"
)

func runMapLive(ctx context.Context, u ui, cfg kmap.LiveConfig, listenAddr string, stdout, stderr io.Writer) int {
	liveSrv, err := kmap.NewLiveServer(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "kavach map: %v\n", err)
		return exitError
	}
	defer liveSrv.Close()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(stderr, "kavach map: listen %s: %v\n", listenAddr, err)
		return exitError
	}
	defer ln.Close()

	liveURL := "http://" + ln.Addr().String()
	u.printBanner(stdout)
	u.header(stdout, "KAVACH MAP (LIVE)")
	fmt.Fprintf(stdout, "%s%s\n", u.label("live", 10), liveURL)
	fmt.Fprintf(stdout, "%s%s\n", u.label("watch", 10), strings.Join(cfg.WatchDirs, ", "))
	fmt.Fprintf(stdout, "%s%s\n", u.label("interval", 10), cfg.Interval)

	liveSrv.Start(ctx)

	httpSrv := &http.Server{
		Handler:           liveSrv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		_ = liveSrv.Close()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		<-errCh
		return exitPass
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "kavach map: serve: %v\n", err)
			return exitError
		}
		return exitPass
	}
}
