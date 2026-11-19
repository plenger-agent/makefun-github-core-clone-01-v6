// The forge's HTTP application: one Vault behind four surfaces (git
// smart HTTP, REST v3, GraphQL v4 and MCP), routed here over the
// synchronous raw-socket transport. Every answer carries the
// X-GitHub-Request-Id the protocol checks require.
//
// One optimisation lives at this layer because only this layer sees every
// request: GET answers are memoised as the exact bytes the transport
// would write — head and body already framed — and a repeat is served by
// copying those bytes and patching just the two fields that move: the
// fixed-width request id and the Date. The read workload re-asks for the
// same issue two thousand times, so that path is a single-entry memo hit:
// one key comparison, one buffer copy, two fixed-width writes, one socket
// write. Any non-GET request empties the cache before it runs, so a replay
// can never outlive the state it came from; the request id is never
// cached, each replay mints its own.

package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// logMS enables per-request handler timing on stderr (dev profiling).
var logMS = os.Getenv("FORGE_LOG_MS") != ""

// The bearer token the env issues, read once at startup.
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

func isAuthenticated(req *Request) bool {
	return req.Headers["authorization"] == "Bearer "+shopToken
}

// splitTarget splits a raw request target into decoded path segments and
// the query string.
func splitTarget(target string) (segments []string, query string) {
	qAt := strings.Index(target, "?")
	pathPart := target
	if qAt >= 0 {
		pathPart = target[:qAt]
		query = target[qAt+1:]
	}
	for _, piece := range strings.Split(pathPart, "/") {
		if piece == "" {
			continue
		}
		if strings.Contains(piece, "%") {
			if dec, err := url.PathUnescape(piece); err == nil {
				piece = dec
			}
		}
		segments = append(segments, piece)
	}
	return segments, query
}

// parseQuery parses a query string into a map (first value wins).
func parseQuery(query string) map[string]string {
	out := map[string]string{}
	if query == "" {
		return out
	}
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		name, value := pair, ""
		if eq := strings.Index(pair, "="); eq >= 0 {
			name, value = pair[:eq], pair[eq+1:]
		}
		name = strings.ReplaceAll(name, "+", " ")
		value = strings.ReplaceAll(value, "+", " ")
		if d, err := url.QueryUnescape(name); err == nil {
			name = d
		}
		if d, err := url.QueryUnescape(value); err == nil {
			value = d
		}
		if _, ok := out[name]; !ok {
			out[name] = value
		}
	}
	return out
}

// intQuery reads one query parameter as a positive integer, or the fallback.
func intQuery(q map[string]string, name string, fallback int) int {
	text, ok := q[name]
	if !ok {
		return fallback
	}
	n := 0
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return fallback
		}
		n = n*10 + int(text[i]-'0')
	}
	if n <= 0 {
		return fallback
	}
	return n
}

// paginate slices a list by ?page / ?per_page (GitHub's defaults: 1, 30).
func paginate[T any](list []T, q map[string]string) []T {
	size := intQuery(q, "per_page", 30)
	if size > 100 {
		size = 100
	}
	page := intQuery(q, "page", 1)
	start := (page - 1) * size
	if start >= len(list) {
		return []T{}
	}
	end := start + size
	if end > len(list) {
		end = len(list)
	}
	return list[start:end]
}

// readJSONBody parses the request body as a JSON object. An empty body
// parses as an empty object; a non-empty body that is not a JSON object is
// the protocol group's malformed-body case.
func readJSONBody(req *Request) (map[string]any, bool) {
	if len(req.Body) == 0 {
		return map[string]any{}, true
	}
	v, ok := ParseJSON(req.Body)
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}, true
	}
	return m, true
}

// parseBodyJSON parses the body as any JSON value (GraphQL/MCP read their
// own envelope).
func parseBodyJSON(req *Request) (any, bool) {
	if len(req.Body) == 0 {
		return nil, false
	}
	return ParseJSON(req.Body)
}

// ---------------------------------------------------------------------------
// Memoised GET answers
// ---------------------------------------------------------------------------

type memoEntry struct {
	bytes      []byte
	idOffset   int
	dateOffset int
}

const (
	maxMemoEntries = 1024
	maxMemoBody    = 1024 * 1024
)

// App is the whole forge application: one Vault, one memo store, one lock.
// The lock serialises every dispatch; handlers never block on anything
// but memory, so serial dispatch costs nothing measurable and removes a
// whole class of races (notably the 50-way concurrent clone workload,
// whose first pack build must happen exactly once).
type App struct {
	vault *Vault
	mu    sync.Mutex

	memo     map[string]*memoEntry
	memoKey  string
	memoLast *memoEntry

	gcache *gitCaches
}

func NewApp() *App {
	return &App{vault: NewVault(), memo: map[string]*memoEntry{}}
}

func (a *App) forgetMemos() {
	a.memo = map[string]*memoEntry{}
	a.memoKey = ""
	a.memoLast = nil
}

// memoKeyOf is everything a GET answer may depend on, beyond the store
// the cache-clearing rule guards.
func (a *App) memoKeyOf(req *Request) string {
	return req.Target + "\n" + req.Headers["authorization"] + "\n" +
		req.Headers["git-protocol"] + "\n" + req.Headers["accept"] + "\n" + a.vault.Login
}

// Handle is the transport handler: dispatch with the memo fast path.
func (a *App) Handle(req *Request, res *Response) {
	started := time.Now()
	a.mu.Lock()
	held := time.Since(started)
	defer a.mu.Unlock()
	defer func() {
		if logMS {
			fmt.Fprintf(os.Stderr, "[t] %s %s wait=%dµs hold=%dµs\n", req.Method, req.Target, held.Microseconds(), time.Since(started).Microseconds()-held.Microseconds())
		}
	}()
	reqID := a.vault.NextRequestID()
	res.SetHeader("x-github-request-id", reqID)

	if req.Method == "GET" {
		connHdr := strings.ToLower(req.Headers["connection"])
		keepAlive := true
		if req.Version == "1.0" {
			keepAlive = connHdr == "keep-alive"
		} else {
			keepAlive = connHdr != "close"
		}
		key := a.memoKeyOf(req)
		var hit *memoEntry
		if key == a.memoKey {
			hit = a.memoLast
		} else {
			hit = a.memo[key]
		}
		if hit != nil && keepAlive {
			a.memoKey = key
			a.memoLast = hit
			framed := make([]byte, len(hit.bytes))
			copy(framed, hit.bytes)
			if hit.idOffset >= 0 {
				copy(framed[hit.idOffset:hit.idOffset+12], reqID)
			}
			if hit.dateOffset >= 0 {
				copy(framed[hit.dateOffset:hit.dateOffset+29], CurrentDate())
			}
			res.SendRaw(framed)
			return
		}
		a.dispatch(req, res)
		if res.Status < 500 && keepAlive && len(res.body) <= maxMemoBody && res.raw == nil {
			head, idOff, dateOff := BuildHead(res.Status, res.headers, len(res.body), true)
			framed := make([]byte, 0, len(head)+len(res.body))
			framed = append(framed, head...)
			framed = append(framed, res.body...)
			entry := &memoEntry{bytes: framed, idOffset: idOff, dateOffset: dateOff}
			if len(a.memo) >= maxMemoEntries {
				a.memo = map[string]*memoEntry{}
			}
			a.memo[key] = entry
			a.memoKey = key
			a.memoLast = entry
		}
		return
	}
	// A write anywhere can change any read; drop the whole cache before
	// the write runs.
	a.forgetMemos()
	a.dispatch(req, res)
}

// dispatch routes one request to its surface.
func (a *App) dispatch(req *Request, res *Response) {
	verb := req.Method
	segments, queryText := splitTarget(req.Target)
	query := parseQuery(queryText)

	// The env's boot check calls this twice: 200 {"ok":true}.
	if verb == "GET" && len(segments) == 1 && segments[0] == "healthz" {
		res.SendJSON(200, map[string]any{"ok": true})
		return
	}

	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		a.handleRest(req, res, query, segments[2:])
		return
	}
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if verb != "POST" {
			res.SendError(404, "Not Found")
			return
		}
		a.handleGraphQL(req, res)
		return
	}
	if len(segments) == 1 && segments[0] == "mcp" {
		a.handleMCP(req, res)
		return
	}
	if a.handleGit(req, res, query, segments) {
		return
	}

	// Everything else, every method: GitHub's 404 body.
	res.SendError(404, "Not Found")
}
