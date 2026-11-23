// Command hobbs serves the aviation data layer over MCP.
//
//	hobbs                 serve over stdio (for a local MCP client)
//	hobbs -http :8080     serve over Streamable HTTP (hosted); needs HOBBS_TOKENS
//
// Configuration is by environment variable; nothing secret is read from files
// or flags:
//
//	HOBBS_TOKENS        comma-separated bearer tokens for the HTTP transport (each >= 16 chars)
//	HOBBS_RATE/BURST    global request rate limit for HTTP (default 5/s, burst 10)
//	NMS_HOST, NMS_CLIENT_ID, NMS_CLIENT_SECRET, NMS_ENV
//	                    FAA NMS-API (NOTAMs); NMS_ENV=production only for the operational service
//	PORT                listen port when -http is not given but a platform provides one
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/mcpserver"
	"github.com/davidhinkes/hobbs/internal/source/awc"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

func main() {
	// Logs go to stderr: on the stdio transport stdout carries the protocol.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	addr := flag.String("http", "", "listen address for the hosted Streamable HTTP server (e.g. :8080); empty serves MCP over stdio")
	flag.Parse()
	if err := run(log, *addr); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var nmsClient *nms.Client
	if cfg, ok := nms.ConfigFromEnv(); ok {
		c, err := nms.NewClient(cfg)
		if err != nil {
			return err
		}
		nmsClient = c
		env := "STAGING (non-production: data may be test data)"
		if cfg.Production {
			env = "production"
		}
		log.Info("NOTAM source enabled", "host", cfg.Host, "environment", env)
	} else {
		log.Warn("NMS-API credentials not configured: NOTAM calls will report the source as unavailable")
	}

	srv, err := mcpserver.New(mcpserver.Deps{AWC: awc.New(), NMS: nmsClient})
	if err != nil {
		return err
	}

	if addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		}
	}
	if addr == "" {
		log.Info("serving MCP over stdio")
		return srv.Run(ctx, &mcp.StdioTransport{})
	}

	rate, _ := strconv.ParseFloat(os.Getenv("HOBBS_RATE"), 64)
	burst, _ := strconv.Atoi(os.Getenv("HOBBS_BURST"))
	handler, err := mcpserver.Handler(srv, mcpserver.HTTPConfig{
		Tokens: strings.Split(os.Getenv("HOBBS_TOKENS"), ","), RatePerSecond: rate, Burst: burst,
	})
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("serving MCP over HTTP", "addr", addr, "endpoint", "/mcp")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
