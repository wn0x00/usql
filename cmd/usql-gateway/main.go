package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "github.com/xo/usql/internal"
	"github.com/xo/usql/internal/ipassgateway"
)

const defaultListenAddress = "127.0.0.1:18768"

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	listenAddress := strings.TrimSpace(os.Getenv("USQL_GATEWAY_LISTEN_ADDR"))
	if listenAddress == "" {
		listenAddress = defaultListenAddress
	}
	if _, _, err := net.SplitHostPort(listenAddress); err != nil {
		logger.Error("invalid gateway listen address")
		return 2
	}

	remoteListener := !ipassgateway.IsLoopbackListenAddress(listenAddress)
	if !listenAddressPermitted(listenAddress, os.Getenv("USQL_GATEWAY_ALLOW_REMOTE")) {
		logger.Error("non-loopback listening requires USQL_GATEWAY_ALLOW_REMOTE=true")
		return 2
	}
	allowedDrivers := parseDriverAllowlist(os.Getenv("USQL_GATEWAY_ALLOWED_DRIVERS"))
	if len(allowedDrivers) == 0 {
		logger.Error("USQL_GATEWAY_ALLOWED_DRIVERS is required")
		return 2
	}
	opener, err := ipassgateway.NewDBURLOpener(allowedDrivers)
	if err != nil {
		logger.Error("database driver allowlist is invalid or unavailable in this build")
		return 2
	}
	handler, err := ipassgateway.NewHandler(ipassgateway.Config{
		Opener: opener,
		Logger: logger,
	})
	if err != nil {
		logger.Error("gateway configuration is invalid")
		return 2
	}

	tlsCertificate := strings.TrimSpace(os.Getenv("USQL_GATEWAY_TLS_CERT_FILE"))
	tlsKey := strings.TrimSpace(os.Getenv("USQL_GATEWAY_TLS_KEY_FILE"))
	if (tlsCertificate == "") != (tlsKey == "") {
		logger.Error("both TLS certificate and key files must be configured")
		return 2
	}

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      195 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownContext.Done()
		context, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(context)
	}()

	logger.Info(
		"database gateway starting",
		slog.String("listen_address", listenAddress),
		slog.Any("allowed_drivers", allowedDrivers),
		slog.Bool("tls_enabled", tlsCertificate != ""),
		slog.Bool("remote_listening_enabled", remoteListener),
	)
	if tlsCertificate != "" {
		err = server.ListenAndServeTLS(tlsCertificate, tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("database gateway stopped unexpectedly")
		return 1
	}
	logger.Info("database gateway stopped")
	return 0
}

func parseDriverAllowlist(value string) []string {
	seen := make(map[string]struct{})
	for _, part := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func listenAddressPermitted(address, allowRemote string) bool {
	return ipassgateway.IsLoopbackListenAddress(address) ||
		strings.EqualFold(strings.TrimSpace(allowRemote), "true")
}
