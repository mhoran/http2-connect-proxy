package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

var debugLog *log.Logger

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
	debugLog.Printf("Read %d bytes from proxy", n)
	return n, nil
}

func (sc *spyConnection) Write(b []byte) (int, error) {
	n := len(b)
	debugLog.Printf("Wrote %d bytes to proxy", n)
	return sc.Conn.Write(b)
}

type WriteCounter struct {
	Message string
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	debugLog.Printf(wc.Message, n)
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

func copyProxy(url *url.URL, tr *http2.Transport, conn net.Conn, pr io.ReadCloser, done, doneError chan bool) {
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
		doneError <- true
		return
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		doneError <- true
		return
	}

	src := io.TeeReader(res.Body, &WriteCounter{
		Message: fmt.Sprintf("Wrote %%d bytes to client %v\n", conn.RemoteAddr().String()),
	})
	_, err = io.Copy(conn, src)
	if err != nil {
		msg := err.Error()
		if err := errors.Unwrap(err); err != nil {
			msg = err.Error()
		}
		log.Printf("Client %v got error in io.Copy(conn, res.Body): %v", conn.RemoteAddr().String(), msg)
		doneError <- true
		return
	}
	done <- true
}

func copyClient(url *url.URL, conn net.Conn, pw *io.PipeWriter, done chan bool) {
	defer func() {
		done <- true
	}()
	src := io.TeeReader(conn, &WriteCounter{
		Message: fmt.Sprintf("Read %%d bytes from client %v\n", conn.RemoteAddr().String()),
	})

	// FIXME: remove when Envoy supports PROXY header
	r := bufio.NewReader(src)

	header := ""
	localIP := getLocalIP(url.Host)
	remotePort := getRemotePort(conn)

	if localIP != nil && remotePort != 0 {
		header = fmt.Sprintf("PROXY TCP4 %v 127.0.0.1 %v 3306\r\n", localIP, remotePort)
	}

	// Block sending header until client sends data
	_, err := r.Peek(1)
	if err != nil {
		return
	}

	io.Copy(pw, io.MultiReader(strings.NewReader(header), r))
}

func handleConnection(url *url.URL, tr *http2.Transport, conn net.Conn) {
	done := make(chan bool, 2)
	doneError := make(chan bool, 1)

	pr, pw := io.Pipe()

	go copyProxy(url, tr, conn, pr, done, doneError)
	go copyClient(url, conn, pw, done)

	select {
	case <-done:
	case <-doneError:
		if conn, ok := conn.(*net.TCPConn); ok {
			conn.SetLinger(0)
		}
	}
	conn.Close()
	pw.Close()
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
	var debug bool
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	var backend string
	flag.StringVar(&backend, "backend", "", "URL to Envoy proxy (required)")
	var port string
	flag.StringVar(&port, "port", "3306", "port to listen on")
	flag.Parse()

	if backend == "" {
		fmt.Println("-backend flag is required")
		os.Exit(1)
	}

	if debug {
		debugLog = log.New(os.Stderr, log.Prefix(), log.Flags())
	} else {
		debugLog = log.New(ioutil.Discard, log.Prefix(), log.Flags())
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

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%s", port))
	if err != nil {
		// handle error
		log.Fatal(err)
	}
	log.Printf("Listening on %v\n", ln.Addr().String())
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
