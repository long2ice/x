package relayx

import (
	"net"

	"github.com/go-gost/x/internal/util/mux"
)

type muxSession struct {
	conn    net.Conn
	session *mux.Session
}

func (s *muxSession) GetConn() (net.Conn, error) {
	return s.session.GetConn()
}

func (s *muxSession) Close() error {
	if s.session == nil {
		return nil
	}
	return s.session.Close()
}

func (s *muxSession) IsClosed() bool {
	if s.session == nil {
		return false
	}
	return s.session.IsClosed()
}

func (s *muxSession) NumStreams() int {
	if s.session == nil {
		return 0
	}
	return s.session.NumStreams()
}
