package wireguard

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	wgdevice "golang.zx2c4.com/wireguard/device"
)

// Conn wraps a Device as a net.Conn carrying raw IP packets.
type Conn struct {
	dev       *Device
	wgDev     *wgdevice.Device
	laddr     net.Addr
	raddr     net.Addr
	ctx       context.Context
	cancel    context.CancelFunc
	onClose   func()
	closeOnce sync.Once
}

func NewConn(dev *Device, wgDev *wgdevice.Device, laddr, raddr net.Addr, ctx context.Context, cancel context.CancelFunc, onClose func()) *Conn {
	return &Conn{
		dev:     dev,
		wgDev:   wgDev,
		laddr:   laddr,
		raddr:   raddr,
		ctx:     ctx,
		cancel:  cancel,
		onClose: onClose,
	}
}

func (c *Conn) Read(b []byte) (int, error)  { return c.dev.ReadPacket(b) }
func (c *Conn) Write(b []byte) (int, error) { return c.dev.WritePacket(b) }

func (c *Conn) LocalAddr() net.Addr  { return c.laddr }
func (c *Conn) RemoteAddr() net.Addr { return c.raddr }

func (c *Conn) SetDeadline(time.Time) error      { return errDeadline() }
func (c *Conn) SetReadDeadline(time.Time) error  { return errDeadline() }
func (c *Conn) SetWriteDeadline(time.Time) error { return errDeadline() }

func errDeadline() error {
	return &net.OpError{Op: "set", Net: "wireguard", Err: errors.New("deadline not supported")}
}

func (c *Conn) Close() error {
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

func (c *Conn) Context() context.Context { return c.ctx }

// Addr is a net.Addr describing a wireguard endpoint by name.
type Addr struct {
	Name string
}

func (a *Addr) Network() string { return "wireguard" }
func (a *Addr) String() string  { return a.Name }
