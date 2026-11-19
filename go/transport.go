// Lean raw-socket HTTP/1.1 transport.
//
// The suite's hot paths are thousands of small sequential requests on
// pooled keep-alive connections, and a general-purpose HTTP stack spends
// most of each request on machinery this forge never uses (header object
// churn, date/connection bookkeeping, stream plumbing in both directions).
// This module speaks just enough HTTP/1.1, directly on net.Conn, to serve
// the suite's clients:
//
//   - request line + headers, Content-Length and chunked request bodies,
//     Expect: 100-continue;
//   - keep-alive with strictly ordered per-connection dispatch (clients
//     never pipeline, but a pipelined request already sitting in the buffer
//     is still answered in order);
//   - one socket write per response (status line, headers, body in a
//     single byte slice);
//   - TCP_NODELAY on every connection, so a small response never waits on
//     Nagle;
//   - a Date header minted once a second and reused, instead of formatting
//     a fresh date for every response.
//
// Two choices keep the per-request cost near the floor. First, the head
// is parsed in place on the bytes the buffered reader already holds: one
// peek, one terminator scan, no per-line reader calls and no copy of the
// whole head (the line-by-line reader stays only as a fallback for heads
// split across segments). Second, response headers live in a plain slice
// scanned linearly — a response carries at most a handful of headers, so
// a scan beats a map plus a lower-casing allocation on every set. A
// request is parsed, dispatched and answered inside one turn of the read
// loop, with the whole body already buffered, so there is no
// reader/writer dance and no goroutine hop between receiving a request
// and answering it.

package main

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is one parsed HTTP request: method, raw target, lower-cased
// headers, whole body.
type Request struct {
	Method  string
	Target  string
	Version string // "1.0" | "1.1"
	Headers map[string]string
	Body    []byte
}

// Response is one response under construction: status, headers, body.
type Response struct {
	Status  int
	headers [][2]string
	body    []byte
	raw     []byte // prebuilt framed response, when set
	done    bool
}

func NewResponse() *Response {
	return &Response{Status: 200}
}

// sameHeader reports whether two header names match ignoring ASCII case,
// without allocating (header names are short; a byte-wise fold is
// cheaper than lower-casing a copy).
func sameHeader(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// headerAt finds a header by name (case-insensitive) in the slice, or -1.
func (r *Response) headerAt(name string) int {
	for i, h := range r.headers {
		if sameHeader(h[0], name) {
			return i
		}
	}
	return -1
}

func (r *Response) SetHeader(name, value string) {
	if at := r.headerAt(name); at >= 0 {
		r.headers[at][1] = value
		return
	}
	r.headers = append(r.headers, [2]string{name, value})
}

func (r *Response) GetHeader(name string) (string, bool) {
	if at := r.headerAt(name); at >= 0 {
		return r.headers[at][1], true
	}
	return "", false
}

// Send is a one-shot answer: status, content type, body.
func (r *Response) Send(status int, body []byte, contentType string) {
	r.Status = status
	r.SetHeader("content-type", contentType)
	r.SetHeader("content-length", strconv.Itoa(len(body)))
	r.body = body
	r.done = true
}

// SendRaw installs a prebuilt, already-framed response to write verbatim.
func (r *Response) SendRaw(framed []byte) {
	r.raw = framed
	r.done = true
}

// JSON answers with a pre-built JSON string (no re-serialization).
func (r *Response) SendJSONText(status int, text string) {
	r.Status = status
	r.SetHeader("content-type", "application/json; charset=utf-8")
	r.SetHeader("content-length", strconv.Itoa(len(text)))
	r.body = []byte(text)
	r.done = true
}

// JSON answers from a value.
func (r *Response) SendJSON(status int, v any) {
	r.SendJSONText(status, Marshal(v)+"\n")
}

// Empty answers with a bare status and no body.
func (r *Response) SendEmpty(status int) {
	r.Status = status
	r.SetHeader("content-length", "0")
	r.body = nil
	r.done = true
}

// Error answers with GitHub's error body: {"message": ...}.
func (r *Response) SendError(status int, message string) {
	r.SendJSON(status, map[string]any{"message": message})
}

type Handler func(req *Request, res *Response)

var statusReasons = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func reasonOf(status int) string {
	if s, ok := statusReasons[status]; ok {
		return s
	}
	return ""
}

// ---------------------------------------------------------------------------
// Date header, cached per second
// ---------------------------------------------------------------------------

var (
	dateMu     sync.Mutex
	dateSecond int64 = -1
	dateText   string
)

// CurrentDate returns the IMF-fixdate for this second, cached.
func CurrentDate() string {
	sec := time.Now().Unix()
	dateMu.Lock()
	defer dateMu.Unlock()
	if sec != dateSecond {
		dateSecond = sec
		dateText = time.Unix(sec, 0).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	}
	return dateText
}

// BuildHead serializes one response head exactly as the wire sees it,
// plus the byte offsets of the request-id and Date values inside it (both
// fixed-width, so a memoised head can be patched in place: the id is
// always FORGE- plus six base-36 digits, the date is always the 29-byte
// IMF-fixdate). Offsets are -1 when the header is absent.
func BuildHead(status int, headers [][2]string, bodyLen int, keepAlive bool) (head string, idOffset, dateOffset int) {
	idOffset, dateOffset = -1, -1
	var b strings.Builder
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(reasonOf(status))
	b.WriteString("\r\n")
	hasLength := false
	hasDate := false
	for _, h := range headers {
		if sameHeader(h[0], "content-length") {
			hasLength = true
		}
		if sameHeader(h[0], "date") {
			hasDate = true
		}
		if sameHeader(h[0], "connection") {
			continue // the transport speaks for the connection
		}
		if sameHeader(h[0], "x-github-request-id") {
			idOffset = b.Len() + len(h[0]) + 2
		}
		b.WriteString(h[0])
		b.WriteString(": ")
		b.WriteString(h[1])
		b.WriteString("\r\n")
	}
	if !hasLength {
		b.WriteString("Content-Length: ")
		b.WriteString(strconv.Itoa(bodyLen))
		b.WriteString("\r\n")
	}
	if !hasDate {
		dateOffset = b.Len() + len("Date: ")
		b.WriteString("Date: ")
		b.WriteString(CurrentDate())
		b.WriteString("\r\n")
	}
	if keepAlive {
		b.WriteString("Connection: keep-alive\r\n")
	} else {
		b.WriteString("Connection: close\r\n")
	}
	b.WriteString("\r\n")
	return b.String(), idOffset, dateOffset
}

const maxHeadBytes = 64 * 1024
const idleTimeout = 65 * time.Second

// ServeConn serves one connection: parse, dispatch in order, answer,
// repeat while keep-alive lasts.
func ServeConn(conn net.Conn, handler Handler) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(conn, 65536)
	// The read deadline is a per-connection idle guard, not a per-request
	// one: refreshing it every request costs a setsockopt per request for
	// nothing. Set it once, then push it forward only when half the idle
	// window has passed, so an active connection never dies mid-suite
	// while an idle one still times out.
	lastDeadlineSet := time.Now()
	conn.SetReadDeadline(lastDeadlineSet.Add(idleTimeout))
	for {
		if time.Since(lastDeadlineSet) > idleTimeout/2 {
			lastDeadlineSet = time.Now()
			conn.SetReadDeadline(lastDeadlineSet.Add(idleTimeout))
		}
		head, rest, err := readHead(br)
		if err != nil {
			return
		}
		_ = rest
		body, keepAlive, ok := readBody(conn, br, head)
		if !ok {
			return
		}
		req := &Request{
			Method:  head.method,
			Target:  head.target,
			Version: head.version,
			Headers: head.headers,
			Body:    body,
		}
		res := NewResponse()
		func() {
			defer func() {
				if recover() != nil {
					if !res.done {
						res.SendError(500, "Internal Server Error")
					}
				}
			}()
			handler(req, res)
		}()
		if !res.done {
			res.SendEmpty(res.Status)
		}
		if !writeResponse(conn, req, res, keepAlive) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

func writeResponse(conn net.Conn, req *Request, res *Response, keepAlive bool) bool {
	if res.raw != nil {
		_, err := conn.Write(res.raw)
		return err == nil
	}
	head, _, _ := BuildHead(res.Status, res.headers, len(res.body), keepAlive)
	var out []byte
	if req.Method == "HEAD" || len(res.body) == 0 {
		out = []byte(head)
	} else {
		out = make([]byte, 0, len(head)+len(res.body))
		out = append(out, head...)
		out = append(out, res.body...)
	}
	_, err := conn.Write(out)
	return err == nil
}

type parsedHead struct {
	method  string
	target  string
	version string
	headers map[string]string
}

// parseHeadBytes turns one head's raw bytes (request line + header lines,
// terminator included) into a parsedHead. Lines are \n-terminated with
// an optional \r; the terminator itself is the empty line at the end.
// Header lines without a colon are ignored, as are blank lines; a
// repeated header joins its values with ", ".
func parseHeadBytes(head []byte) (*parsedHead, error) {
	// Request line: the first line.
	eol := bytes.IndexByte(head, '\n')
	if eol < 0 {
		return nil, errMalformed
	}
	reqLine := bytes.TrimSuffix(head[:eol], []byte("\r"))
	sp1 := bytes.IndexByte(reqLine, ' ')
	sp2 := -1
	if sp1 >= 0 {
		rest := reqLine[sp1+1:]
		if j := bytes.IndexByte(rest, ' '); j >= 0 {
			sp2 = sp1 + 1 + j
		}
	}
	if sp1 < 0 || sp2 < 0 {
		return nil, errMalformed
	}
	versionText := reqLine[sp2+1:]
	if !bytes.HasPrefix(versionText, []byte("HTTP/")) {
		return nil, errMalformed
	}
	headers := map[string]string{}
	pos := eol + 1
	for pos < len(head) {
		end := bytes.IndexByte(head[pos:], '\n')
		if end < 0 {
			break
		}
		line := bytes.TrimSuffix(head[pos:pos+end], []byte("\r"))
		pos += end + 1
		if len(line) == 0 {
			break
		}
		colon := bytes.IndexByte(line, ':')
		if colon > 0 {
			name := strings.ToLower(strings.TrimSpace(string(line[:colon])))
			value := strings.TrimSpace(string(line[colon+1:]))
			if prev, ok := headers[name]; ok {
				headers[name] = prev + ", " + value
			} else {
				headers[name] = value
			}
		}
	}
	return &parsedHead{
		method:  string(reqLine[:sp1]),
		target:  string(reqLine[sp1+1 : sp2]),
		version: string(versionText[len("HTTP/"):]),
		headers: headers,
	}, nil
}

// readHead reads one request head from the buffered reader. Fast path:
// the head usually arrives in a single segment, so peek at what is
// already buffered, find the blank-line terminator, parse in place and
// discard exactly the head bytes. Slow path: the head is split across
// segments (or larger than the peek), so fall back to reading it line by
// line. The peek consumes nothing, so the fallback still sees every byte.
func readHead(br *bufio.Reader) (*parsedHead, []byte, error) {
	// Peek only ever asks for bytes already buffered: Peek(n) waits until
	// n bytes arrive, so asking for a large n would stall on a head that
	// arrived whole in one segment. First block for a single byte, then
	// look at everything that has landed so far.
	if br.Buffered() == 0 {
		if _, err := br.Peek(1); err != nil {
			return nil, nil, err
		}
	}
	if n := br.Buffered(); n > 0 {
		peeked, _ := br.Peek(n)
		end := bytes.Index(peeked, []byte("\r\n\r\n"))
		termLen := 4
		if end < 0 {
			end = bytes.Index(peeked, []byte("\n\n"))
			termLen = 2
		}
		if end >= 0 {
			consumed := end + termLen
			parsed, err := parseHeadBytes(peeked[:consumed])
			if err != nil {
				return nil, nil, err
			}
			br.Discard(consumed)
			return parsed, nil, nil
		}
	}
	var buf []byte
	for {
		if len(buf) > maxHeadBytes {
			return nil, nil, errTooLarge
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, nil, err
		}
		buf = append(buf, line...)
		if len(buf) >= 4 && bytes.Equal(buf[len(buf)-4:], []byte("\r\n\r\n")) {
			break
		}
		if len(buf) >= 2 && bytes.Equal(buf[len(buf)-2:], []byte("\n\n")) {
			break
		}
	}
	parsed, err := parseHeadBytes(buf)
	return parsed, nil, err
}

// readBody reads the request body per Content-Length or chunked framing.
// With Expect: 100-continue it sends the interim reply before the body
// arrives, so a client waiting on it starts sending.
func readBody(conn net.Conn, br *bufio.Reader, head *parsedHead) ([]byte, bool, bool) {
	connHdr := strings.ToLower(head.headers["connection"])
	keepAlive := true
	if head.version == "1.0" {
		keepAlive = connHdr == "keep-alive"
	} else {
		keepAlive = connHdr != "close"
	}
	te := strings.ToLower(head.headers["transfer-encoding"])
	if strings.Contains(te, "chunked") {
		body, ok := readChunked(br)
		return body, keepAlive, ok
	}
	length := 0
	if cl, ok := head.headers["content-length"]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, keepAlive, false
		}
		length = n
	}
	if length == 0 {
		return nil, keepAlive, true
	}
	// The client may be waiting on 100-continue before sending the body.
	if strings.ToLower(head.headers["expect"]) == "100-continue" && br.Buffered() < length {
		conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	body := make([]byte, length)
	n := 0
	for n < length {
		m, err := br.Read(body[n:])
		n += m
		if err != nil {
			return nil, keepAlive, false
		}
	}
	return body, keepAlive, true
}

func readChunked(br *bufio.Reader) ([]byte, bool) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, false
		}
		sizeText := strings.TrimSpace(strings.Split(line, ";")[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, false
		}
		if size == 0 {
			// Consume the trailing CRLF (and any trailers up to blank line).
			for {
				trail, err := br.ReadString('\n')
				if err != nil {
					return nil, false
				}
				if trail == "\r\n" || trail == "\n" {
					break
				}
			}
			return out, true
		}
		chunk := make([]byte, size+2)
		n := 0
		for n < len(chunk) {
			m, err := br.Read(chunk[n:])
			n += m
			if err != nil {
				return nil, false
			}
		}
		out = append(out, chunk[:size]...)
	}
}

type transportError string

func (e transportError) Error() string { return string(e) }

const (
	errTooLarge  = transportError("request head too large")
	errMalformed = transportError("malformed request line")
)
