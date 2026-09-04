// Command cloudpath-driver-stcb 是 STC-B Driver Plugin 的独立进程入口。
//
// 它读取 CloudPath Plugin Host 经环境变量注入的 launch identity，输出唯一握手行，
// 拨号 Host 的 loopback 端点，并在该认证传输上服务 Driver Protocol v1。
// 传输由 pluginmain 注入，因此本二进制不自行监听、也不自行等待对端。
package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
	"github.com/DeliciousBuding/cloud-path/sdk/go/pluginmain"
	"github.com/DeliciousBuding/cloud-path/sdk/go/rpc"
	"github.com/DeliciousBuding/cloud-path/sdk/go/transport"

	"github.com/DeliciousBuding/cloud-path-driver-stcb/plugin"
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

	if err := pluginmain.Run(ctx, os.Stdout, os.Stderr, func(tr transport.Transport) *rpc.Server {
		return driver.NewRPCServer(tr, &shutdownAwareDriver{Driver: plugin.New(), onShutdown: exitAfterShutdown})
	}); err != nil {
		os.Exit(1)
	}
}

type shutdownAwareDriver struct {
	*plugin.Driver
	onShutdown func()
}

func (d *shutdownAwareDriver) Shutdown(ctx context.Context, req *driver.ShutdownRequest) (*driver.ShutdownResponse, error) {
	resp, err := d.Driver.Shutdown(ctx, req)
	if d.onShutdown != nil {
		d.onShutdown()
	}
	return resp, err
}
