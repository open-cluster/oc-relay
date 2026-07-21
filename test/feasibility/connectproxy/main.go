// Minimal controllable HTTP CONNECT proxy for the transport feasibility gate: simulates a
// corporate egress proxy. It tunnels TLS byte-for-byte (never touching the HTTP/2
// inside — the property the gate verifies) and exposes a control port that drops
// every active tunnel on demand (the interruption scenario) plus an optional idle
// timeout (the idle-kill scenario).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type tunnelSet struct {
	mu      sync.Mutex
	tunnels map[int]net.Conn
	nextID  int
}

func (t *tunnelSet) add(conn net.Conn) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	t.tunnels[t.nextID] = conn
	return t.nextID
}

func (t *tunnelSet) remove(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tunnels, id)
}

func (t *tunnelSet) dropAll() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	dropped := 0
	for id, conn := range t.tunnels {
		conn.Close()
		delete(t.tunnels, id)
		dropped++
	}
	return dropped
}

func main() {
	var listenAddr, controlAddr string
	var idleTimeout time.Duration
	flag.StringVar(&listenAddr, "listen", "127.0.0.1:18081", "proxy listen address")
	flag.StringVar(&controlAddr, "control", "127.0.0.1:18082", "control listen address (any connection drops all tunnels)")
	flag.DurationVar(&idleTimeout, "idle-timeout", 0, "kill tunnels idle longer than this (0 = never)")
	flag.Parse()

	tunnels := &tunnelSet{tunnels: map[int]net.Conn{}}

	go controlLoop(controlAddr, tunnels)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("connectproxy: listening %s (control %s, idle-timeout %s)\n",
		listenAddr, controlAddr, idleTimeout)

	for {
		client, err := listener.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			return
		}
		go serve(client, tunnels, idleTimeout)
	}
}

func controlLoop(addr string, tunnels *tunnelSet) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control listen: %v\n", err)
		os.Exit(1)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		dropped := tunnels.dropAll()
		fmt.Printf("connectproxy: DROPPED %d tunnel(s) on control trigger\n", dropped)
		fmt.Fprintf(conn, "dropped %d\n", dropped)
		conn.Close()
	}
}

func serve(client net.Conn, tunnels *tunnelSet, idleTimeout time.Duration) {
	defer client.Close()

	request, err := http.ReadRequest(bufio.NewReader(client))
	if err != nil {
		return
	}
	if request.Method != http.MethodConnect {
		fmt.Fprintf(client, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}

	upstream, err := net.DialTimeout("tcp", request.Host, 10*time.Second)
	if err != nil {
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	id := tunnels.add(client)
	defer tunnels.remove(id)
	fmt.Printf("connectproxy: tunnel %d -> %s\n", id, request.Host)

	watchdog := func(conn net.Conn) io.Writer {
		if idleTimeout <= 0 {
			return conn
		}
		return deadlineWriter{conn: conn, timeout: idleTimeout}
	}

	done := make(chan struct{}, 2)
	copyHalf := func(dst net.Conn, src net.Conn) {
		io.Copy(watchdog(dst), src)
		done <- struct{}{}
	}
	go copyHalf(upstream, client)
	go copyHalf(client, upstream)
	<-done

	if !strings.Contains(request.Host, "control") {
		fmt.Printf("connectproxy: tunnel %d closed\n", id)
	}
}

// deadlineWriter enforces an idle timeout: every write refreshes the deadline; a
// quiet tunnel gets killed by the connection deadline, like a real middlebox.
type deadlineWriter struct {
	conn    net.Conn
	timeout time.Duration
}

func (w deadlineWriter) Write(p []byte) (int, error) {
	w.conn.SetDeadline(time.Now().Add(w.timeout))
	return w.conn.Write(p)
}
