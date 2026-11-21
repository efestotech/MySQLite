package wire

import "net"

// hostOnlyConn reports the client address without the port. go-mysql uses
// RemoteAddr().String() only to build error messages (1045, 1449 and the SSL refusal),
// where MySQL prints the host alone: 'test'@'127.0.0.1', not 'test'@'127.0.0.1:52144'.
// Logs use the raw connection's address, which keeps the port.
type hostOnlyConn struct {
	net.Conn
}

func (c hostOnlyConn) RemoteAddr() net.Addr {
	return hostOnlyAddr{c.Conn.RemoteAddr()}
}

type hostOnlyAddr struct {
	net.Addr
}

func (a hostOnlyAddr) String() string {
	s := a.Addr.String()
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}
