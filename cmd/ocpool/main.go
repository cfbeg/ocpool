package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"ocpool/internal/app"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:10000", "HTTP listen address")
	root := flag.String("root", ".", "Project root containing scripts and .state")
	flag.Parse()
	a, e := app.New(*root)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	defer a.Close()
	_ = a.AccessFile(*listen)
	server := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	stopped := make(chan os.Signal, 1)
	// Containers send SIGTERM during a normal stop. Handle it the same way as
	// Ctrl-C so in-flight requests and child OpenCode processes get the normal
	// bounded shutdown path instead of being hard-killed by the runtime.
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-stopped:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		}
	}()
	fmt.Printf("OCPool listening on http://%s\nLogin link: %s/access-url.txt\n", *listen, a.State)
	if e = server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, e)
	}
}
