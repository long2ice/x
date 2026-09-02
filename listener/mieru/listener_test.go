package mieru

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	"github.com/go-gost/core/listener"
	xlogger "github.com/go-gost/x/logger"
)

type acceptResult struct {
	conn net.Conn
	req  *model.Request
	err  error
}

type fakeMieruServer struct {
	results []acceptResult
	running bool
	accepts int
}

func (s *fakeMieruServer) Load() (*mieruserver.ServerConfig, error) { return nil, nil }
func (s *fakeMieruServer) Store(*mieruserver.ServerConfig) error    { return nil }
func (s *fakeMieruServer) Start() error                             { s.running = true; return nil }
func (s *fakeMieruServer) Stop() error                              { s.running = false; return nil }
func (s *fakeMieruServer) IsRunning() bool                          { return s.running }
func (s *fakeMieruServer) Accept() (net.Conn, *model.Request, error) {
	result := s.results[s.accepts]
	s.accepts++
	return result.conn, result.req, result.err
}

func TestAcceptContinuesAfterSessionError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	server := &fakeMieruServer{
		running: true,
		results: []acceptResult{
			{err: io.EOF},
			{conn: serverConn, req: &model.Request{}},
		},
	}
	ln := &mieruListener{
		server: server,
		log:    xlogger.Nop(),
	}

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	defer conn.Close()
	if server.accepts != 2 {
		t.Fatalf("server Accept() calls = %d, want 2", server.accepts)
	}
}

func TestAcceptReturnsClosedWhenServerStops(t *testing.T) {
	server := &fakeMieruServer{
		results: []acceptResult{{err: io.EOF}},
	}
	ln := &mieruListener{
		server: server,
		log:    xlogger.Nop(),
	}

	conn, err := ln.Accept()
	if conn != nil {
		conn.Close()
		t.Fatal("Accept() returned a connection after server stopped")
	}
	if !errors.Is(err, listener.ErrClosed) {
		t.Fatalf("Accept() error = %v, want %v", err, listener.ErrClosed)
	}
}
