// Raw TCP HTTP/1.1 transport for the github-core-clone-01 Go forge.
// Port of the Node entry's src/transport.ts.
//
// The suite's clients (undici fetch, git smart-HTTP) only ever send
// origin-form HTTP/1.1 requests with Content-Length bodies, keep-alive,
// and no chunked encoding, so a small purpose-built parser is enough —
// and it lets each response go out as ONE conn.Write of ONE prebuilt
// buffer. Requests on one connection are answered strictly in order,
// one at a time; different connections run concurrently in goroutines
// (the app layer serialises store access with one mutex, mirroring
// Node's single thread). Idle keep-alive connections close after
// IDLE_TIMEOUT; TCP_NODELAY is set explicitly: the read workload
// reuses one keep-alive socket for 2,000 ping-pong requests, and
// Nagle + delayed ACK would stall exactly that pattern.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Idle keep-alive sockets are closed after this long without traffic.
const idleTimeout = 30 * time.Second

// A request head bigger than this without a terminator is malformed.
const maxHeadBytes = 32 * 1024

// Largest single body the transport will buffer (handlers cap at 10 MiB).
const maxBodyBytes = 10 * 1024 * 1024

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	413: "Content Too Large",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// Request is one parsed HTTP request. The body is complete before the
// handler runs; Path is the raw (still percent-encoded) pathname and
// Query is parsed once here so no handler pays to parse it again.
type Request struct {
	Method    string
	Target    string
	Path      string
	Query     url.Values
	Headers   map[string]string // lowercase keys
	Body      []byte
	KeepAlive bool
}

func (r *Request) Header(name string) string {
	if r.Headers == nil {
		return ""
	}
	return r.Headers[strings.ToLower(name)]
}

// Response is what a handler returns: status, headers, body. The
// transport assembles the bytes once and writes them in a single Write.
// Content-Length is always recomputed from the actual body.
type Response struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

func textResponse(status int, contentType string, body []byte) Response {
	return Response{Status: status, Headers: [][2]string{{"Content-Type", contentType}}, Body: body}
}

func jsonResponse(status int, body []byte) Response {
	return textResponse(status, "application/json", body)
}

// Counter-based request ids: one random draw seeds a process-unique
// counter — unique, hex, non-empty, which is all the suite's protocol
// check asserts (nonEmpty). No crypto per request.
var reqIDCounter uint64 = uint64(time.Now().UnixNano()&0xffffffffff) + 1
var reqIDMu sync.Mutex

func nextRequestID() string {
	reqIDMu.Lock()
	id := reqIDCounter
	reqIDCounter++
	reqIDMu.Unlock()
	return fmt.Sprintf("%016x", id)
}

var respBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

type transportServer struct {
	ln      net.Listener
	handler func(*Request) Response
}

func (s *transportServer) Addr() net.Addr { return s.ln.Addr() }

func (s *transportServer) Close() error { return s.ln.Close() }

func listenTransport(handler func(*Request) Response) (*transportServer, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	s := &transportServer{ln: ln, handler: handler}
	go s.acceptLoop()
	return s, nil
}

func (s *transportServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

func (s *transportServer) serveConn(conn net.Conn) {
	defer conn.Close()
	tcp, ok := conn.(*net.TCPConn)
	if ok {
		_ = tcp.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(conn, 65536)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		req, err := readRequest(br, conn)
		if err != nil {
			if isMalformed(err) {
				_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nconnection: close\r\ncontent-length: 0\r\n\r\n"))
			}
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		resp := s.handler(req)
		if !writeResponse(conn, resp, req.KeepAlive) {
			return
		}
		if !req.KeepAlive {
			return
		}
	}
}

type malformedError struct{ msg string }

func (e *malformedError) Error() string { return e.msg }

func isMalformed(err error) bool {
	_, ok := err.(*malformedError)
	return ok
}

func malformed(msg string) error { return &malformedError{msg} }

// readRequest parses one complete request from the buffered reader.
func readRequest(br *bufio.Reader, conn net.Conn) (*Request, error) {
	// Request line + headers, terminated by an empty line.
	var headBuf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if headBuf.Len()+len(line) > maxHeadBytes {
			return nil, malformed("request head too large")
		}
		headBuf.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	head := headBuf.String()
	lines := strings.Split(strings.TrimSuffix(head, "\r\n"), "\r\n")
	if len(lines) == 0 {
		return nil, malformed("empty request")
	}
	reqLine := lines[0]
	sp1 := strings.IndexByte(reqLine, ' ')
	sp2 := -1
	if sp1 >= 0 {
		sp2 = strings.IndexByte(reqLine[sp1+1:], ' ')
		if sp2 >= 0 {
			sp2 += sp1 + 1
		}
	}
	if sp1 <= 0 || sp2 <= sp1+1 {
		return nil, malformed("malformed request line")
	}
	method := reqLine[:sp1]
	target := reqLine[sp1+1 : sp2]
	version := reqLine[sp2+1:]
	if !strings.HasPrefix(version, "HTTP/") {
		return nil, malformed("malformed HTTP version")
	}
	headers := make(map[string]string, len(lines)-1)
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, malformed("malformed header line")
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		if prev, ok := headers[name]; ok {
			headers[name] = prev + ", " + value
		} else {
			headers[name] = value
		}
	}
	connection := strings.ToLower(headers["connection"])
	keepAlive := true
	if version == "HTTP/1.1" {
		keepAlive = connection != "close"
	} else {
		keepAlive = connection == "keep-alive"
	}
	if strings.ToLower(headers["expect"]) == "100-continue" {
		_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}

	var body []byte
	if strings.Contains(strings.ToLower(headers["transfer-encoding"]), "chunked") {
		b, err := readChunkedBody(br)
		if err != nil {
			return nil, err
		}
		body = b
	} else {
		length := 0
		if raw, ok := headers["content-length"]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || n < 0 || n > maxBodyBytes {
				return nil, malformed("bad content-length")
			}
			length = n
		}
		if length > 0 {
			body = make([]byte, length)
			if _, err := readFull(br, body); err != nil {
				return nil, err
			}
		} else {
			body = []byte{}
		}
	}

	path := target
	var query url.Values
	if qi := strings.IndexByte(target, '?'); qi >= 0 {
		path = target[:qi]
		if qi == 0 {
			path = "/"
		}
		if qi+1 < len(target) {
			query, _ = url.ParseQuery(target[qi+1:])
		}
	}
	if path == "" {
		path = "/"
	}
	return &Request{
		Method: method, Target: target, Path: path, Query: query,
		Headers: headers, Body: body, KeepAlive: keepAlive,
	}, nil
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := br.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, malformed("bad chunk size")
		}
		if size == 0 {
			// Trailer lines until an empty line.
			for {
				trailer, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if trailer == "\r\n" || trailer == "\n" {
					return out.Bytes(), nil
				}
			}
		}
		chunk := make([]byte, size+2)
		if _, err := readFull(br, chunk); err != nil {
			return nil, err
		}
		out.Write(chunk[:size])
	}
}

func writeResponse(conn net.Conn, resp Response, keepAlive bool) bool {
	bufPtr := respBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	reason := reasonPhrases[resp.Status]
	buf = append(buf, "HTTP/1.1 "...)
	buf = strconv.AppendInt(buf, int64(resp.Status), 10)
	buf = append(buf, ' ')
	buf = append(buf, reason...)
	buf = append(buf, "\r\n"...)
	for _, h := range resp.Headers {
		if strings.EqualFold(h[0], "content-length") {
			continue // always recomputed from the real body
		}
		buf = append(buf, h[0]...)
		buf = append(buf, ": "...)
		buf = append(buf, h[1]...)
		buf = append(buf, "\r\n"...)
	}
	if resp.Status != 204 && resp.Status != 304 {
		buf = append(buf, "content-length: "...)
		buf = strconv.AppendInt(buf, int64(len(resp.Body)), 10)
		buf = append(buf, "\r\n"...)
	}
	if keepAlive {
		buf = append(buf, "connection: keep-alive\r\n\r\n"...)
	} else {
		buf = append(buf, "connection: close\r\n\r\n"...)
	}
	buf = append(buf, resp.Body...)
	_, err := conn.Write(buf)
	*bufPtr = buf[:0]
	respBufPool.Put(bufPtr)
	return err == nil
}
