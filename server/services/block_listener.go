package services

import (
	"net"
)

type BlockListener struct {
	ln        net.Listener
	blockedIP []net.IP
}

func NewBlockListener(l net.Listener, blockedIP []net.IP) *BlockListener {
	ln := &BlockListener{
		ln:        l,
		blockedIP: blockedIP,
	}
	return ln
}

func (ln *BlockListener) Accept() (net.Conn, error) {
	c, err := ln.ln.Accept()
	if err != nil {
		return c, err
	}
	if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		for _, ip := range ln.blockedIP {
			if addr.IP.String() == ip.String() {
				c.Close()
				return c, nil
			}
		}
	}
	return c, err
}

func (ln *BlockListener) Close() error {
	return ln.ln.Close()
}

func (ln *BlockListener) Addr() net.Addr {
	return ln.ln.Addr()
}
