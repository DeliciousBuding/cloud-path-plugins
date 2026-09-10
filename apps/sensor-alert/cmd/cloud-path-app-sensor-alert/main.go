// SPDX-License-Identifier: Apache-2.0

// Command cloud-path-app-sensor-alert serves the public Application Protocol v1
// over the authenticated transport injected by the host. It chooses no endpoint
// and never opens hardware, serial ports or network sockets.
package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	sensoralert "github.com/DeliciousBuding/cloud-path-app-sensor-alert"
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
				time.Sleep(100 * time.Millisecond)
				stop()
			}()
		})
	}

	svc := sensoralert.New()
	if err := pluginmain.Run(ctx, os.Stdout, os.Stderr, func(tr transport.Transport) *rpc.Server {
		return application.NewRPCServer(tr, &shutdownAwareService{
			ApplicationServer: svc,
			onShutdown:        exitAfterShutdown,
		})
	}); err != nil {
		os.Exit(1)
	}
}

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
