package wireguard

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	wgdevice "golang.zx2c4.com/wireguard/device"
)

// conn adapts a wgDevice to net.Conn so the downstream handler reads and
// writes raw IP packets just like it would with a TUN device.
type conn struct {
	dev       *wgDevice
	wgDev     *wgdevice.Device
	laddr     net.Addr
	raddr     net.Addr
	ctx       context.Context
	cancel    context.CancelFunc
	onClose   func()
	closeOnce sync.Once
}

func (c *conn) Read(b []byte) (int, error)  { return c.dev.readPacket(b) }
func (c *conn) Write(b []byte) (int, error) { return c.dev.writePacket(b) }

func (c *conn) LocalAddr() net.Addr  { return c.laddr }
func (c *conn) RemoteAddr() net.Addr { return c.raddr }

func (c *conn) SetDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "wireguard", Err: errors.New("deadline not supported")}
}
func (c *conn) SetReadDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "wireguard", Err: errors.New("deadline not supported")}
}
func (c *conn) SetWriteDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "wireguard", Err: errors.New("deadline not supported")}
}

func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}

func (c *conn) Context() context.Context { return c.ctx }

type addr struct {
	name string
}

func (a *addr) Network() string { return "wireguard" }
func (a *addr) String() string  { return a.name }
