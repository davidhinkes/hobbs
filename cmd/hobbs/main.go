// Command hobbs serves the aviation data layer over MCP.
//
//	hobbs                          serve over stdio (weather only), not airworthy
//	hobbs -nms staging             also serve NOTAMs from the NMS-API staging environment
//	hobbs -http :8080 -nms staging serve over Streamable HTTP (hosted)
//
// Configuration is by command-line flag. Secrets are the exception: they are
// read from environment variables only, so they never appear in process
// listings or shell history:
//
//	HOBBS_TOKENS        comma-separated bearer tokens for -http (each >= 16 chars)
//	HOBBS_NMS_CLIENT_ID       FAA NMS-API credentials, needed when -nms is not "off"
//	HOBBS_NMS_CLIENT_SECRET
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
	airworthy bool
	http      string
	rate      float64
	rateSet   bool
	nms       string
}

func main() {
	// Logs go to stderr: on the stdio transport stdout carries the protocol.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var o options
	flag.BoolVar(&o.airworthy, "airworthy", false, "declare responses usable for flight planning and navigation (airworthy: true). Off by default: every response then says airworthy: false. Refused with -nms staging")
	flag.StringVar(&o.http, "http", "", "listen address for the hosted Streamable HTTP server, e.g. :8080 (on Railway: -http :$PORT); empty serves MCP over stdio")
	flag.Float64Var(&o.rate, "rate", 5, "HTTP only: global request rate limit in requests per second (bursts up to twice this); an error without -http")
	flag.StringVar(&o.nms, "nms", "off", `NOTAM source: "off", "staging" or "production" (credentials from HOBBS_NMS_CLIENT_ID / HOBBS_NMS_CLIENT_SECRET)`)
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "rate" {
			o.rateSet = true
		}
	})
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
	id, secret, ok := nms.CredentialsFromEnv()
	if !ok {
		return nil, errors.New("-nms=" + o.nms + " needs HOBBS_NMS_CLIENT_ID and HOBBS_NMS_CLIENT_SECRET in the environment")
	}
	c, err := nms.NewClient(nms.Config{Host: host, ClientID: id, ClientSecret: secret})
	if err != nil {
		return nil, err
	}
	log.Info("NOTAM source enabled", "nms", o.nms, "host", host)
	return c, nil
}

func run(log *slog.Logger, o options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if o.airworthy && o.nms == "staging" {
		return errors.New("-airworthy is refused: NOTAMs from staging are test data and cannot be airworthy")
	}
	if o.rateSet && o.http == "" {
		return errors.New("-rate applies only to the HTTP server: use it with -http, or leave it out")
	}
	if o.rate <= 0 {
		return errors.New("-rate must be greater than 0")
	}
	if o.airworthy {
		log.Warn("AIRWORTHY mode: responses are declared usable for flight planning and navigation")
	} else {
		log.Info("not airworthy (default): every response says airworthy: false")
	}
	n, err := nmsClient(log, o)
	if err != nil {
		return err
	}
	srv, err := mcpserver.New(mcpserver.Deps{AWC: awc.New(), NMS: n, Airworthy: o.airworthy})
	if err != nil {
		return err
	}

	if o.http == "" {
		log.Info("serving MCP over stdio")
		return srv.Run(ctx, &mcp.StdioTransport{})
	}

	handler, err := mcpserver.Handler(srv, mcpserver.HTTPConfig{
		Tokens: strings.Split(os.Getenv("HOBBS_TOKENS"), ","), RatePerSecond: o.rate,
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
