// Command hobbs serves the aviation data layer over MCP.
//
//	hobbs                          serve over stdio (weather only)
//	hobbs -nms staging             also serve NOTAMs from the NMS-API staging environment
//	hobbs -http :8080 -nms staging serve over Streamable HTTP (hosted)
//
// Configuration is by command-line flag. Secrets are the exception: they are
// read from environment variables only, so they never appear in process
// listings or shell history:
//
//	HOBBS_TOKENS        comma-separated bearer tokens for -http (each >= 16 chars)
//	NMS_CLIENT_ID       FAA NMS-API credentials, needed when -nms is not "off"
//	NMS_CLIENT_SECRET
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/mcpserver"
	"github.com/davidhinkes/hobbs/internal/source/awc"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

type options struct {
	http    string
	rate    float64
	burst   int
	nms     string
	nmsHost string
}

func main() {
	// Logs go to stderr: on the stdio transport stdout carries the protocol.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var o options
	flag.StringVar(&o.http, "http", "", "listen address for the hosted Streamable HTTP server, e.g. :8080 (on Railway: -http :$PORT); empty serves MCP over stdio")
	flag.Float64Var(&o.rate, "rate", 5, "HTTP only: global request rate limit, requests per second")
	flag.IntVar(&o.burst, "burst", 10, "HTTP only: global request burst")
	flag.StringVar(&o.nms, "nms", "off", `NOTAM source: "off", "staging" or "production" (credentials from NMS_CLIENT_ID / NMS_CLIENT_SECRET)`)
	flag.StringVar(&o.nmsHost, "nms-host", "", "override the NMS-API host (scheme and host only)")
	flag.Parse()
	if err := run(log, o); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// nmsClient builds the NOTAM client the flags ask for, or nil when -nms=off.
func nmsClient(log *slog.Logger, o options) (*nms.Client, error) {
	var host string
	switch o.nms {
	case "off":
		if _, _, ok := nms.CredentialsFromEnv(); ok {
			log.Info("NMS credentials are set but -nms=off: NOTAM calls will report the source as not configured")
		} else {
			log.Info("NOTAMs off (-nms=off): NOTAM calls will report the source as not configured")
		}
		return nil, nil
	case "staging":
		host = nms.StagingHost
	case "production":
		host = nms.ProductionHost
	default:
		return nil, fmt.Errorf("-nms must be off, staging or production, not %q", o.nms)
	}
	if o.nmsHost != "" {
		host = strings.TrimRight(o.nmsHost, "/")
	}
	id, secret, ok := nms.CredentialsFromEnv()
	if !ok {
		return nil, errors.New("-nms=" + o.nms + " needs NMS_CLIENT_ID and NMS_CLIENT_SECRET in the environment")
	}
	c, err := nms.NewClient(nms.Config{Host: host, ClientID: id, ClientSecret: secret, Production: o.nms == "production"})
	if err != nil {
		return nil, err
	}
	env := "STAGING (non-production: data may be test data)"
	if o.nms == "production" {
		env = "production"
	}
	log.Info("NOTAM source enabled", "host", host, "environment", env)
	return c, nil
}

func run(log *slog.Logger, o options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := nmsClient(log, o)
	if err != nil {
		return err
	}
	srv, err := mcpserver.New(mcpserver.Deps{AWC: awc.New(), NMS: n})
	if err != nil {
		return err
	}

	if o.http == "" {
		log.Info("serving MCP over stdio")
		return srv.Run(ctx, &mcp.StdioTransport{})
	}

	handler, err := mcpserver.Handler(srv, mcpserver.HTTPConfig{
		Tokens: strings.Split(os.Getenv("HOBBS_TOKENS"), ","), RatePerSecond: o.rate, Burst: o.burst,
	})
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: o.http, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("serving MCP over HTTP", "addr", o.http, "endpoint", "/mcp")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
