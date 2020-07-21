package yamux

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

func (s *Session) handleWithRecover(keepalive bool) {
	for {
		var wg sync.WaitGroup
		ctx, cancelFn := context.WithCancel(context.Background())
		go s.waitToDie(recv, ctx, &wg)
		go s.waitToDie(send, ctx, &wg)
		if keepalive {
			go s.waitToDie(keepaliveFn, ctx, &wg)
		}
		expected := <-s.exitCh
		cancelFn()
		wg.Wait()
		if expected {
			return
		}

		timeout := time.NewTimer(20 * time.Second)
		// Once reach here, the recv and send has dead,
		// wait to recover them
		// WARNING: There may be race if Ping() called close
		// and new Conn comes in at same time
		select {
		case c, ok := <-s.newConnCh:
			if !ok {
				return
			}
			s.conn = c
			continue
		case <-timeout.C:
			return
		case <-s.shutdownCh:
			return
		}
	}
}

func recv(s *Session, ctx context.Context) error {
	hdr := header(make([]byte, headerSize))
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		// Read the header
		if _, err := io.ReadFull(s.bufRead, hdr); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "reset by peer") {
				s.logger.Printf("[ERR] yamux: Failed to read header: %v", err)
			} else {
				err = nil
			}
			return err
		}

		// Verify the version
		if hdr.Version() != protoVersion {
			s.logger.Printf("[ERR] yamux: Invalid protocol version: %d", hdr.Version())
			return ErrInvalidVersion
		}

		mt := hdr.MsgType()
		if mt < typeData || mt > typeGoAway {
			return ErrInvalidMsgType
		}

		if err := handlers[mt](s, hdr); err != nil {
			return err
		}
	}

}

func send(s *Session, ctx context.Context) error {
	for {
		select {
		case ready := <-s.sendCh:
			// Send a header if ready
			if ready.Hdr != nil {
				sent := 0
				for sent < len(ready.Hdr) {
					n, err := s.conn.Write(ready.Hdr[sent:])
					if err != nil {
						s.logger.Printf("[ERR] yamux: Failed to write header: %v", err)
						asyncSendErr(ready.Err, err)
						return err
					}
					sent += n
				}
			}

			// Send data from a body if given
			if ready.Body != nil {
				_, err := io.Copy(s.conn, ready.Body)
				if err != nil {
					s.logger.Printf("[ERR] yamux: Failed to write body: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
			}

			// No error, successful send
			asyncSendErr(ready.Err, nil)
		case <-s.shutdownCh:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func keepaliveFn(s *Session, ctx context.Context) error {
	for {
		select {
		case <-time.After(s.config.KeepAliveInterval):
			_, err := s.Ping()
			if err != nil {
				if err != ErrSessionShutdown {
					s.logger.Printf("[ERR] yamux: keepalive failed: %v", err)
					err = ErrKeepAliveTimeout
				} else {
					err = nil
				}
				return err
			}
		case <-s.shutdownCh:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Session) waitToDie(fn func(s *Session, ctx context.Context) error, ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	// return nil:should wait recover, error:should stop
	if err := fn(s, ctx); err == nil {
		s.exitCh <- true
	} else {
		s.exitCh <- false
	}
	wg.Done()
}

func (s *Session) ReplaceConn(conn net.Conn) {
	s.newConnCh <- conn
}

func (s *Session) SaveMeta(info []byte) {
	s.metaLock.Lock()
	defer s.metaLock.Unlock()
	s.meta = info
}

func (s *Session) LoadMeta() []byte {
	s.metaLock.RLock()
	defer s.metaLock.RUnlock()
	return s.meta
}
