package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"

	"github.com/GoogleCloudPlatform/cloudsql-proxy/logging"
)

func main() {
	if err := realMain(); err != nil {
		log.Fatalln(err)
	}
}

func realMain() error {
	caPath := flag.String("ca", "testcerts/ca.pem", "MySQL CA Cert path")
	serverCertPath := flag.String("cert", "testcerts/server-cert.pem", "MySQL server Cert path")
	serverKeyPath := flag.String("key", "testcerts/server-key.pem", "MySQL server Key path")

	flag.Parse()

	caBuf, err := ioutil.ReadFile(*caPath)
	if err != nil {
		return err
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caBuf)

	certs, err := tls.LoadX509KeyPair(*serverCertPath, *serverKeyPath)
	if err != nil {
		return err
	}

	localAddr := "127.0.0.1:3308"
	backendAddr := "127.0.0.1:3306"

	log.Printf("listening on %s", localAddr)

	l, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}

	cfg := &tls.Config{
		PreferServerCipherSuites: true,
		MinVersion:               tls.VersionTLS12,
		ClientCAs:                caPool,
		Certificates:             []tls.Certificate{certs},
		// GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {

		// },
	}

	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		tlsConn := tls.Server(c, cfg)

		log.Printf("new connection for %q", backendAddr)

		backendConn, err := net.Dial("tcp", backendAddr) // mysql instance
		if err != nil {
			return fmt.Errorf("couldn't connect to backend: %s", err)
		}

		copyThenClose(backendConn, tlsConn, "remote conn", "local conn on "+backendAddr)
	}
}

func copyThenClose(remote, local io.ReadWriteCloser, remoteDesc, localDesc string) {
	firstErr := make(chan error, 1)

	go func() {
		readErr, err := myCopy(remote, local)
		select {
		case firstErr <- err:
			if readErr && err == io.EOF {
				logging.Verbosef("Client closed %v", localDesc)
			} else {
				copyError(localDesc, remoteDesc, readErr, err)
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
			logging.Verbosef("Instance %v closed connection", remoteDesc)
		} else {
			copyError(remoteDesc, localDesc, readErr, err)
		}
		remote.Close()
		local.Close()
	default:
		// In this case, the other goroutine exited first and already printed its
		// error (and closed the things).
	}
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

func copyError(readDesc, writeDesc string, readErr bool, err error) {
	var desc string
	if readErr {
		desc = "Reading data from " + readDesc
	} else {
		desc = "Writing data to " + writeDesc
	}
	log.Printf("%v had error: %v", desc, err)
}
