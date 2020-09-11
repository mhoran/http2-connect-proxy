package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/http2"
)

var debug bool

func WrapConnection(c net.Conn) net.Conn {
	return &spyConnection{
		Conn: c,
	}
}

type spyConnection struct {
	net.Conn
}

func (sc *spyConnection) Read(b []byte) (int, error) {
	n, err := sc.Conn.Read(b)
	if err != nil {
		return n, err
	}
	if debug {
		log.Printf("Read %d bytes from proxy", n)
	}
	return n, nil
}

func (sc *spyConnection) Write(b []byte) (int, error) {
	n := len(b)
	if debug {
		log.Printf("Wrote %d bytes to proxy", n)
	}
	return sc.Conn.Write(b)
}

type WriteCounter struct {
	Message string
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	if debug {
		log.Printf(wc.Message, n)
	}
	return n, nil
}

func getLocalIP(host string) net.IP {
	conn, err := net.Dial("udp", host)
	defer conn.Close()
	if err != nil {
		return nil
	}
	if localAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return localAddr.IP
	}
	return nil
}

func getRemotePort(conn net.Conn) int {
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

func handleConnection(url *url.URL, tr *http2.Transport, conn net.Conn) {
	reset := false
	defer func() {
		if reset {
			// Set SO_LINGER to 0 to send RST on conn.Close()
			conn.(*net.TCPConn).SetLinger(0)
		}
		conn.Close()
	}()

	pr, pw := io.Pipe()
	closePipe := true
	defer func() {
		if closePipe {
			pw.Close()
		}
	}()

	req := &http.Request{
		Method: "CONNECT",
		URL:    url,
		Host:   "127.0.0.1:3306",
		Body:   pr,
	}

	// Send the request
	//res, err := c.Do(req)
	res, err := tr.RoundTrip(req)
	if err != nil {
		log.Printf("Error in tr.RoundTrip: %v", err)
		reset = true
		return
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		reset = true
		return
	}

	var localIP = getLocalIP(url.Host)
	var remotePort = getRemotePort(conn)

	// FIXME: remove when Envoy supports PROXY header
	if localIP != nil && remotePort != 0 {
		_, err := fmt.Fprintf(pw, "PROXY TCP4 %v 127.0.0.1 %v 3306\r\n", localIP, remotePort)
		if err != nil {
			return
		}
	}

	closePipe = false
	go func() {
		defer pw.Close()
		src := io.TeeReader(conn, &WriteCounter{
			Message: fmt.Sprintf("Read %%d bytes from client %v\n", conn.RemoteAddr().String()),
		})
		_, err = io.Copy(pw, src)
	}()

	src := io.TeeReader(res.Body, &WriteCounter{
		Message: fmt.Sprintf("Wrote %%d bytes to client %v\n", conn.RemoteAddr().String()),
	})
	_, err = io.Copy(conn, src)
	if err != nil {
		log.Printf("Client %v got error in io.Copy(conn, res.Body): %v", conn.RemoteAddr().String(), err)
		if err := errors.Unwrap(err); err != nil {
			switch err.(type) {
			case http2.ConnectionError:
			case http2.GoAwayError:
			case http2.StreamError:
				reset = true
			}
		}
	}
}

func addKeyLogWriter(cfg *tls.Config) {
	fn := os.Getenv("SSLKEYLOGFILE")
	if fn != "" {
		w, err := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err == nil {
			cfg.KeyLogWriter = w
		}
	}
}

func main() {
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	var backend string
	flag.StringVar(&backend, "backend", "", "URL to Envoy proxy (required)")
	flag.Parse()

	if backend == "" {
		fmt.Println("-backend flag is required")
		os.Exit(1)
	}

	url, err := url.Parse(backend)
	if err != nil {
		log.Fatal(err)
	}

	dial := func(network, addr string, cfg *tls.Config) (net.Conn, error) {
		log.Printf("Connecting to %s\n", addr)
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		addKeyLogWriter(cfg)
		conn, err := tls.DialWithDialer(dialer, network, addr, cfg)
		if err != nil {
			return nil, err
		}
		return WrapConnection(conn), nil
	}
	tr := &http2.Transport{DialTLS: dial, ReadIdleTimeout: 60 * time.Second}
	//c := &http.Client{Transport: transport}

	ln, err := net.Listen("tcp", "127.0.0.1:3306")
	if err != nil {
		// handle error
		log.Fatal(err)
	}
	log.Println("Listening on 127.0.0.1:3306")
	for {
		conn, err := ln.Accept()
		if err != nil {
			// handle error
			log.Fatal(err)
		}
		log.Printf("Client connected: %v\n", conn.RemoteAddr().String())
		go handleConnection(url, tr, conn)
	}

}
