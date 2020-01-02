package gracenet

import (
	"net"
	"time"
)

type GraceListener struct {
	ln      net.Listener
	expCh   chan bool
	revCh   chan bool
	actCh   chan bool
	running bool
	expired bool
	gt      *time.Timer
	d       time.Duration
}

func NewGraceListener(l net.Listener, d time.Duration) *GraceListener {
	gt := time.NewTimer(d)
	actCh := make(chan bool)
	expCh := make(chan bool)
	revCh := make(chan bool)
	ln := &GraceListener{
		ln:      l,
		expCh:   expCh,
		revCh:   revCh,
		actCh:   actCh,
		running: true,
		expired: false,
		gt:      gt,
		d:       d,
	}
	go func() {
		for {
			select {
			case <-ln.gt.C:
				if !ln.expired && ln.running {
					ln.expired = true
					ln.expCh <- true
				}
			case <-ln.actCh:
				if ln.expired && ln.running {
					ln.expired = false
					ln.revCh <- true
				} else {
					ln.gt.Reset(ln.d)
				}
			}
		}
	}()
	return ln
}

func (ln *GraceListener) Expire() <-chan bool {
	return ln.expCh
}

func (ln *GraceListener) Revoke() <-chan bool {
	return ln.revCh
}

func (ln *GraceListener) Stop() {
	ln.running = false
}

func (ln *GraceListener) Start() {
	ln.running = true
	ln.gt.Reset(ln.d)
}

func (ln *GraceListener) Accept() (net.Conn, error) {
	c, err := ln.ln.Accept()
	if err != nil {
		return c, err
	}
	c = NewGraceConn(c, ln.actCh)
	return c, err
}

func (ln *GraceListener) Close() error {
	// close(ln.revCh)
	// close(ln.actCh)
	// close(ln.expCh)
	return ln.ln.Close()
}

func (ln *GraceListener) Addr() net.Addr {
	return ln.ln.Addr()
}
