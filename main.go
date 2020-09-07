package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
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

// what happens if IP changes?
func main() {
	flag.BoolVar(&debug, "debug", false, "debug logging")
	flag.Parse()

	dial := func(network, addr string, cfg *tls.Config) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		conn, err := tls.DialWithDialer(dialer, network, addr, cfg)
		if err != nil {
			return nil, err
		}
		return WrapConnection(conn), nil
	}
	tr := &http2.Transport{DialTLS: dial}
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
		go func() {
			pr, pw := io.Pipe()

			url, err := url.Parse("https://mysql.example.com:10001")
			if err != nil {
				log.Fatal(err)
			}
			req := &http.Request{
				Method: "CONNECT",
				URL:    url,
				Host:   "127.0.0.1:3306",
				Body:   ioutil.NopCloser(pr),
			}

			// Send the request
			//res, err := c.Do(req)
			res, err := tr.RoundTrip(req)
			if err != nil {
				log.Printf("Error in tr.RoundTrip: %v", err)
				conn.Close()
				return
			}
			if res.StatusCode != 200 {
				conn.Close()
				return
			}

			// FIXME: remove when Envoy supports PROXY header
			if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
				fmt.Fprintf(pw, "PROXY TCP4 127.0.0.1 127.0.0.1 %v 3306\r\n", addr.Port)
			}

			go func() {
				src := io.TeeReader(res.Body, &WriteCounter{
					Message: "Wrote %d bytes to client\n",
				})
				io.Copy(conn, src)

				conn.Close()
			}()
			src := io.TeeReader(conn, &WriteCounter{
				Message: "Read %d bytes from client\n",
			})
			io.Copy(pw, src)
			pw.Close()
		}()
	}

}
