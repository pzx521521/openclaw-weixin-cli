package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	cloud "github.com/pzx521521/openclaw-weixin-cli/internal/cloud"
)

const defaultPort = 7860

func main() {
	if err := run(); err != nil {
		slog.Error("cloud exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	portFlag := flag.Int("port", 0, "listen port (overrides PORT env)")
	webDir := flag.String("web", "web/dist", "frontend static dir")
	pollTimeout := flag.Duration("poll", 60*time.Second, "long poll timeout for account ready wait")
	sendTimeout := flag.Duration("send-timeout", cloud.DefaultSendTimeout, "history fetch timeout for send, 0 disables polling")
	dsnFlag := flag.String("dsn", "", "postgres DSN (overrides PGSTORE_WECHAT_DSN)")
	flag.Parse()

	port := resolvePort(*portFlag)
	dsn := strings.TrimSpace(*dsnFlag)
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("PGSTORE_WECHAT_DSN"))
	}
	if dsn == "" {
		return errMissingDSN()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := cloud.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	srv := cloud.NewServer(store, *pollTimeout, *sendTimeout, *webDir)
	mux := http.NewServeMux()
	srv.Mount(mux)

	go cleanupLoop(store)

	httpSrv := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("cloud listening", "port", port, "web", *webDir)
	return httpSrv.ListenAndServe()
}

// resolvePort prefers -port, then PORT env, then 7860.
func resolvePort(flagPort int) int {
	if flagPort > 0 {
		return flagPort
	}
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultPort
}

func cleanupLoop(store *cloud.Store) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = store.CleanupExpired(ctx)
		cancel()
	}
}

type missingDSNError struct{}

func (missingDSNError) Error() string {
	return "missing postgres DSN: set PGSTORE_WECHAT_DSN env or -dsn flag"
}

func errMissingDSN() error { return missingDSNError{} }
