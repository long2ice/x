package mieru

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/model"
	"github.com/go-gost/core/bypass"
	"github.com/go-gost/core/logger"
)

type udpAssociateOptions struct {
	service string
	bypass  bypass.Bypass
	logger  logger.Logger
}

func runUDPAssociateLoop(ctx context.Context, tunnel *apicommon.PacketOverStreamTunnel, upstream net.PacketConn, resolver apicommon.DNSResolver, opts udpAssociateOptions) error {
	if resolver == nil {
		resolver = &net.Resolver{}
	}

	var udpErr atomic.Value
	var addrMap sync.Map

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			n, err := tunnel.Read(buf)
			if err != nil {
				udpErr.Store(err)
				return
			}

			datagram, err := parseSocks5UDPDatagram(buf[:n])
			if err != nil {
				udpErr.Store(err)
				return
			}

			dstAddr, err := resolveUDPAddr(ctx, resolver, datagram.Addr)
			if err != nil {
				if opts.logger != nil {
					opts.logger.Debugf("UDP associate resolve %v failed: %v", datagram.Addr, err)
				}
				continue
			}

			if opts.bypass != nil && opts.bypass.Contains(ctx, "udp", dstAddr.String(), bypass.WithService(opts.service)) {
				if opts.logger != nil {
					opts.logger.Debug("bypass: ", dstAddr.String())
				}
				continue
			}

			addrMap.Store(dstAddr.String(), datagram.Header)
			if _, err := upstream.WriteTo(datagram.Payload, dstAddr); err != nil {
				if opts.logger != nil {
					opts.logger.Debugf("UDP associate write to %v failed: %v", dstAddr, err)
				}
				continue
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			n, addr, err := upstream.ReadFrom(buf)
			if err != nil {
				if udpErr.Load() == nil {
					udpErr.Store(err)
				}
				return
			}

			var header []byte
			if v, ok := addrMap.Load(addr.String()); ok {
				header = v.([]byte)
			} else {
				header, err = udpAddrHeader(addr)
				if err != nil {
					udpErr.Store(err)
					return
				}
				addrMap.Store(addr.String(), header)
			}

			if _, err := tunnel.Write(append(append([]byte(nil), header...), buf[:n]...)); err != nil {
				if udpErr.Load() == nil {
					udpErr.Store(err)
				}
				return
			}
		}
	}()

	wg.Wait()
	if v := udpErr.Load(); v != nil {
		if err, ok := v.(error); ok {
			return err
		}
	}
	return nil
}

type socks5UDPDatagram struct {
	Addr    model.AddrSpec
	Header  []byte
	Payload []byte
}

func parseSocks5UDPDatagram(pkt []byte) (*socks5UDPDatagram, error) {
	if len(pkt) <= 6 {
		return nil, errors.New("mieru: UDP datagram too short")
	}
	if pkt[0] != 0x00 || pkt[1] != 0x00 {
		return nil, errors.New("mieru: invalid UDP datagram prefix")
	}
	if pkt[2] != 0x00 {
		return nil, fmt.Errorf("mieru: unsupported UDP fragment %d", pkt[2])
	}

	r := bytes.NewReader(pkt[3:])
	dst := model.AddrSpec{}
	if err := dst.ReadFromSocks5(r); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("mieru: truncated UDP datagram")
		}
		return nil, err
	}
	headerLen := len(pkt) - r.Len()
	if headerLen < 0 || headerLen > len(pkt) {
		return nil, errors.New("mieru: invalid UDP datagram header")
	}

	return &socks5UDPDatagram{
		Addr:    dst,
		Header:  append([]byte(nil), pkt[:headerLen]...),
		Payload: append([]byte(nil), pkt[headerLen:]...),
	}, nil
}

func resolveUDPAddr(ctx context.Context, resolver apicommon.DNSResolver, addr model.AddrSpec) (*net.UDPAddr, error) {
	if addr.IP.To4() != nil || addr.IP.To16() != nil {
		return &net.UDPAddr{IP: addr.IP, Port: addr.Port}, nil
	}
	if addr.FQDN != "" {
		return apicommon.ResolveUDPAddr(ctx, resolver, "udp", addr.String())
	}
	return nil, model.ErrUnrecognizedAddrType
}

func udpAddrHeader(addr net.Addr) ([]byte, error) {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("mieru: unexpected address type %T", addr)
	}
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0})
	spec := model.AddrSpec{IP: udpAddr.IP, Port: udpAddr.Port}
	if err := spec.WriteToSocks5(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
