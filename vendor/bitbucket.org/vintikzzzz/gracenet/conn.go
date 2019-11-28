package gracenet

import (
	"net"
	"time"
)

type GraceConn struct {
	c  net.Conn
	ch chan bool
}

func NewGraceConn(c net.Conn, ch chan bool) *GraceConn {
	return &GraceConn{
		c:  c,
		ch: ch,
	}
}

func (c *GraceConn) Read(b []byte) (n int, err error) {
	return c.c.Read(b)
}

func (c *GraceConn) Write(b []byte) (n int, err error) {
	c.ch <- true
	return c.c.Write(b)
}

func (c *GraceConn) Close() error {
	return c.c.Close()
}

func (c *GraceConn) LocalAddr() net.Addr {
	return c.c.LocalAddr()
}

func (c *GraceConn) RemoteAddr() net.Addr {
	return c.c.RemoteAddr()
}

func (c *GraceConn) SetDeadline(t time.Time) error {
	return c.c.SetDeadline(t)
}

func (c *GraceConn) SetReadDeadline(t time.Time) error {
	return c.c.SetReadDeadline(t)
}

func (c *GraceConn) SetWriteDeadline(t time.Time) error {
	return c.c.SetWriteDeadline(t)
}
