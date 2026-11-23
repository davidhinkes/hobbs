// Command hobbs serves the aviation data layer over MCP.
//
//	hobbs                          serve over stdio (weather only), not airworthy
//	hobbs -nms-url https://api-staging.cgifederal-aim.com           also serve NOTAMs from that NMS-API server
//	hobbs -http :8080 -nms-url https://api-staging.cgifederal-aim.com   serve over Streamable HTTP (hosted)
//
// Configuration is by command-line flag. Secrets are the exception: they are
// read from environment variables only, so they never appear in process
// listings or shell history:
//
//	HOBBS_NMS_CLIENT_ID       FAA NMS-API credentials, needed with -nms-url
//	HOBBS_NMS_CLIENT_SECRET
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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
	nmsURL    string
}

func main() {
	// Logs go to stderr: on the stdio transport stdout carries the protocol.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var o options
	flag.BoolVar(&o.airworthy, "airworthy", false, "declare responses usable for flight planning and navigation (airworthy: true). Off by default: every response then says airworthy: false. Accepted only without -nms-url or with the FAA's production NMS-API URL ("+nms.ProductionHost+")")
	flag.StringVar(&o.http, "http", "", "listen address for the hosted Streamable HTTP server, e.g. :8080 (on Railway: -http :$PORT); empty serves MCP over stdio")
	flag.Float64Var(&o.rate, "rate", 5, "HTTP only: global request rate limit in requests per second (bursts up to twice this); an error without -http")
	flag.StringVar(&o.nmsURL, "nms-url", "", "NMS-API server for NOTAMs, scheme and host only (e.g. "+nms.StagingHost+"); empty turns NOTAMs off. Credentials from HOBBS_NMS_CLIENT_ID / HOBBS_NMS_CLIENT_SECRET")
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

// nmsClient builds the NOTAM client for -nms-url, or nil when it is empty.
func nmsClient(log *slog.Logger, o options) (*nms.Client, error) {
	if o.nmsURL == "" {
		if _, _, ok := nms.CredentialsFromEnv(); ok {
			log.Info("NMS credentials are set but -nms-url is empty: NOTAM calls will report the source as not enabled")
		} else {
			log.Info("NOTAMs off (no -nms-url): NOTAM calls will report the source as not enabled")
		}
		return nil, nil
	}
	id, secret, ok := nms.CredentialsFromEnv()
	if !ok {
		return nil, errors.New("-nms-url needs HOBBS_NMS_CLIENT_ID and HOBBS_NMS_CLIENT_SECRET in the environment")
	}
	c, err := nms.NewClient(nms.Config{Host: o.nmsURL, ClientID: id, ClientSecret: secret})
	if err != nil {
		return nil, err
	}
	log.Info("NOTAM source enabled", "url", o.nmsURL)
	return c, nil
}

// checkNMSURL normalizes -nms-url and rejects anything but a scheme and host.
func checkNMSURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", fmt.Errorf("-nms-url must be a scheme and host only, e.g. %s, not %q", nms.StagingHost, raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func run(log *slog.Logger, o options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if o.nmsURL, err = checkNMSURL(o.nmsURL); err != nil {
		return err
	}
	// Only the FAA's production NMS-API can be airworthy; any other server,
	// staging included, is treated as test data.
	if o.airworthy && o.nmsURL != "" && o.nmsURL != nms.ProductionHost {
		return fmt.Errorf("-airworthy is refused: NOTAMs from %s are not the FAA's production NMS-API (%s), so they cannot be airworthy", o.nmsURL, nms.ProductionHost)
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

	handler := mcpserver.Handler(srv, mcpserver.HTTPConfig{RatePerSecond: o.rate})
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
