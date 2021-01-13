package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"
)

const (
	keepAlivePeriod = time.Minute
)

// Cert represents the client certificate key pair in the root certiciate
// authority that the client uses to verify server certificates.
type Cert struct {
	ClientCert tls.Certificate
	CACert     *x509.Certificate
}

// CertSource is used
type CertSource interface {
	// Cert returns the required certs needed to establish a TLS connection
	// from the client to the server.
	Cert(ctx context.Context, db, branch string) (*Cert, error)
}

// Client is responsible for listening to unsecured connections over a TCP
// localhost port and tunneling them securely over a TLS connection to a remote
// database instance.
type Client struct {
	// RemoteAddr defines the address to tunnel local connections
	RemoteAddr string

	// LocalAddr defines the address to listen for new connection
	LocalAddr string

	// Instance defines the remote DB instance to proxy new connection
	Instance string

	// MaxConnections is the maximum number of connections to establish
	// before refusing new connections. 0 means no limit.
	MaxConnections uint64

	// CertSource defines the certificate source to obtain the required TLS
	// certificates for the client.
	CertSource CertSource

	// connectionsCounter is used to enforce the optional maxConnections limit
	connectionsCounter uint64
}

// Conn represents a connection from a client to a specific instance.
type Conn struct {
	Instance string
	Conn     net.Conn
}

// Run runs the proxy. It listens to the configured localhost address and
// proxies the connection over a TLS tunnel to the remote DB instance.
func (c *Client) Run(ctx context.Context) error {
	connSrc := make(chan Conn, 1)
	go func() {
		if err := c.listen(connSrc); err != nil {
			log.Printf("listen error: %s", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			termTimeout := time.Second * 1
			log.Printf("received context cancellation. Waiting up to %s before terminating.", termTimeout)

			err := c.Shutdown(termTimeout)
			if err != nil {
				return fmt.Errorf("error during shutdown: %v", err)
			}
			return nil
		case conn := <-connSrc:
			go func(lc Conn) {
				// TODO(fatih): detach context from parent
				err := c.handleConn(ctx, lc.Conn, lc.Instance)
				if err != nil {
					log.Printf("error proxying conn: %s", err)
				}
			}(conn)
		}
	}
}

func (c *Client) listen(connSrc chan<- Conn) error {
	l, err := net.Listen("tcp", c.LocalAddr)
	if err != nil {
		return fmt.Errorf("error net.Listen: %s", err)
	}

	log.Printf("listening on %q for remote DB instance %q", c.LocalAddr, c.Instance)

	for {
		start := time.Now()
		conn, err := l.Accept()
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				d := 10*time.Millisecond - time.Since(start)
				if d > 0 {
					time.Sleep(d)
				}
				continue
			}
			l.Close()

			return fmt.Errorf("error in accept for on %v: %v", c.LocalAddr, err)
		}

		log.Printf("new connection for %q", c.LocalAddr)

		switch clientConn := conn.(type) {
		case *net.TCPConn:
			clientConn.SetKeepAlive(true)                  //nolint: errcheck
			clientConn.SetKeepAlivePeriod(1 * time.Minute) //nolint: errcheck
		}

		connSrc <- Conn{
			Conn:     conn,
			Instance: c.Instance, // TODO(fatih): fix this
		}
	}
}

func (c *Client) handleConn(ctx context.Context, conn net.Conn, instance string) error {
	active := atomic.AddUint64(&c.connectionsCounter, 1)

	// Deferred decrement of ConnectionsCounter upon connection closing
	defer atomic.AddUint64(&c.connectionsCounter, ^uint64(0))

	if c.MaxConnections > 0 && active > c.MaxConnections {
		conn.Close()
		return fmt.Errorf("too many open connections (max %d)", c.MaxConnections)
	}

	// TODO(fatih): cache certs
	cert, err := c.CertSource.Cert(ctx, instance, "branch")
	if err != nil {
		return fmt.Errorf("couldn't retrieve certs from cert source: %s", err)
	}

	rootCA := x509.NewCertPool()
	rootCA.AddCert(cert.CACert)

	serverName := "MySQL_Server_5.7.32_Auto_Generated_Server_Certificate"
	cfg := &tls.Config{
		ServerName:   serverName,
		Certificates: []tls.Certificate{cert.ClientCert},
		RootCAs:      rootCA,
		// We need to set InsecureSkipVerify to true due to
		// https://github.com/GoogleCloudPlatform/cloudsql-proxy/issues/194
		// https://tip.golang.org/doc/go1.11#crypto/x509
		//
		// Since we have a secure channel to the Cloud SQL API which we use to retrieve the
		// certificates, we instead need to implement our own VerifyPeerCertificate function
		// that will verify that the certificate is OK.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: genVerifyPeerCertificateFunc(serverName, rootCA),
	}

	// TODO(fatih): implement refreshing certs
	// go p.refreshCertAfter(instance, timeToRefresh)

	var d net.Dialer
	remoteConn, err := d.DialContext(ctx, "tcp", c.RemoteAddr)
	if err != nil {
		conn.Close()
		return fmt.Errorf("couldn't connect to %q: %v", c.RemoteAddr, err)
	}

	type setKeepAliver interface {
		SetKeepAlive(keepalive bool) error
		SetKeepAlivePeriod(d time.Duration) error
	}

	if s, ok := conn.(setKeepAliver); ok {
		if err := s.SetKeepAlive(true); err != nil {
			log.Printf("couldn't set KeepAlive to true: %v", err)
		} else if err := s.SetKeepAlivePeriod(keepAlivePeriod); err != nil {
			log.Printf("couldn't set KeepAlivePeriod to %v", keepAlivePeriod)
		}
	} else {
		log.Printf("KeepAlive not supported: long-running tcp connections may be killed by the OS.")
	}

	secureConn := tls.Client(remoteConn, cfg)
	if err := secureConn.Handshake(); err != nil {
		secureConn.Close()
		return fmt.Errorf("couldn't initiate TLS handshake to remote addr: %s", err)
	}

	// Hasta la vista, baby
	copyThenClose(
		secureConn,
		conn,
		"remote connection",
		"local connection on "+conn.LocalAddr().String(),
	)
	return nil
}

// Shutdown waits up to a given amount of time for all active connections to
// close. Returns an error if there are still active connections after waiting
// for the whole length of the timeout.
func (c *Client) Shutdown(timeout time.Duration) error {
	term, ticker := time.After(timeout), time.NewTicker(100*time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if atomic.LoadUint64(&c.connectionsCounter) > 0 {
				continue
			}
			log.Println("no connections to wait, bailing out")
		case <-term:
		}
		break
	}

	active := atomic.LoadUint64(&c.connectionsCounter)
	if active == 0 {
		return nil
	}
	return fmt.Errorf("%d active connections still exist after waiting for %v", active, timeout)
}

// genVerifyPeerCertificateFunc creates a VerifyPeerCertificate func that verifies that the peer
// certificate is in the cert pool. We need to define our own because of our sketchy non-standard
// CNs.
func genVerifyPeerCertificateFunc(instanceName string, pool *x509.CertPool) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no certificate to verify")
		}

		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("x509.ParseCertificate(rawCerts[0]) returned error: %v", err)
		}

		opts := x509.VerifyOptions{Roots: pool}
		if _, err = cert.Verify(opts); err != nil {
			return err
		}

		if cert.Subject.CommonName != instanceName {
			return fmt.Errorf("certificate had CN %q, expected %q", cert.Subject.CommonName, instanceName)
		}
		return nil
	}
}

func copyThenClose(remote, local io.ReadWriteCloser, remoteDesc, localDesc string) {
	firstErr := make(chan error, 1)

	go func() {
		readErr, err := myCopy(remote, local)
		select {
		case firstErr <- err:
			if readErr && err == io.EOF {
				log.Printf("client closed %v", localDesc)
			} else {
				logError(localDesc, remoteDesc, readErr, err)
			}
			remote.Close()
			local.Close()
		default:
		}
	}()

	readErr, err := myCopy(local, remote)
	select {
	case firstErr <- err:
		if readErr && err == io.EOF {
			log.Printf("instance %v closed connection", remoteDesc)
		} else {
			logError(remoteDesc, localDesc, readErr, err)
		}
		remote.Close()
		local.Close()
	default:
		// In this case, the other goroutine exited first and already printed its
		// error (and closed the things).
	}
}

func logError(readDesc, writeDesc string, readErr bool, err error) {
	var desc string
	if readErr {
		desc = "reading data from " + readDesc
	} else {
		desc = "writing data to " + writeDesc
	}
	log.Printf("%v had error: %v", desc, err)
}

// myCopy is similar to io.Copy, but reports whether the returned error was due
// to a bad read or write. The returned error will never be nil
func myCopy(dst io.Writer, src io.Reader) (readErr bool, err error) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				if err == nil {
					return false, werr
				}
				// Read and write error; just report read error (it happened first).
				return true, err
			}
		}
		if err != nil {
			return true, err
		}
	}
}