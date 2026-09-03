// Command cloud-path-app-scheduled-compartment is the executable entrypoint for
// the Scheduled Compartment reference application.
//
// It is an install-style Application plugin: it reads the launch identity
// injected by the A4 Plugin Host through pluginmain, emits the single
// CloudPath handshake line, dials the host's loopback endpoint and serves the
// Application Protocol v1 over that authenticated transport. The transport is
// injected by the host, not selected here.
package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/DeliciousBuding/cloud-path-app-scheduled-compartment"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/pluginmain"
	"github.com/DeliciousBuding/cloud-path/sdk/go/rpc"
	"github.com/DeliciousBuding/cloud-path/sdk/go/transport"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var once sync.Once
	exitAfterShutdown := func() {
		once.Do(func() {
			go func() {
				// Give the RPC dispatcher a moment to flush the Shutdown
				// response before the process exits.
				time.Sleep(100 * time.Millisecond)
				stop()
			}()
		})
	}

	svc := scheduledcompartment.New()
	if err := pluginmain.Run(ctx, os.Stdout, os.Stderr, func(tr transport.Transport) *rpc.Server {
		return application.NewRPCServer(tr, &shutdownAwareService{
			ApplicationServer: svc,
			onShutdown:        exitAfterShutdown,
		})
	}); err != nil {
		os.Exit(1)
	}
}

// shutdownAwareService turns the service's Shutdown RPC into a process exit
// signal, so the host's graceful shutdown path never falls back to a kill.
type shutdownAwareService struct {
	application.ApplicationServer
	onShutdown func()
}

func (s *shutdownAwareService) Shutdown(ctx context.Context, req *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	resp, err := s.ApplicationServer.Shutdown(ctx, req)
	if s.onShutdown != nil {
		s.onShutdown()
	}
	return resp, err
}
