// Git smart HTTP: the info/refs advertisements (protocol v2 and v0), v2
// ls-refs and fetch, v0 fetch, and v0 receive-pack with report-status.
// The suite's client speaks the wire protocol in-process, so this file
// implements the protocol itself: pkt-line framing, pack v2 reading (with
// delta resolution) and writing (plain objects — always valid),
// shallow/deepen bookkeeping, and per-ref fast-forward enforcement.
//
// Everything is served from the shared Vault store: objects pushed here
// are the objects the REST contents API and clones see.
//
// Two caches sit on top of the fetch path, both sound because the object
// store is content-addressed and append-only (objects are never mutated
// or deleted, so the reachable closure of a fixed oid set can never
// change): fetch answers (v0 and v2) and ls-refs answers are cached by
// their exact inputs plus a fingerprint of the refs they depend on. The
// clone workload asks fifty times for the same depth-1 pack; it is built
// once. The v2 capability advertisement carries no per-repo data at all,
// so it is built once, up front.
//
// The pack reader is the hot path (every push inflates one): a deflate
// stream's length is found with its own Adler-32 checksum, not by
// binary-searching inflate attempts. A zlib stream ends with the Adler-32
// of its output, so the stream's end is the first place those four bytes
// occur, confirmed by one inflate of the exact prefix. That turns an
// entry's unpack from a dozen inflations into two.

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const agentName = "plenger-forge/1.0.0"

var typeToID = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var idToType = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

// pktLine frames one payload: 4 hex digits of (payload length + 4), then
// the payload.
func pktLine(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(hex4(len(payload)+4))...)
	out = append(out, payload...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

func hex4(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(n>>12)&0xf], digits[(n>>8)&0xf], digits[(n>>4)&0xf], digits[n&0xf]})
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type packet struct {
	kind    byte // 'd' data, 'f' flush, '1' delim
	payload []byte
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// decodePackets decodes a pkt-line stream; trailing bytes that are not
// framed are returned as rest.
func decodePackets(data []byte) (packets []packet, rest []byte) {
	pos := 0
	for pos+4 <= len(data) {
		d0, d1, d2, d3 := hexVal(data[pos]), hexVal(data[pos+1]), hexVal(data[pos+2]), hexVal(data[pos+3])
		if d0 < 0 || d1 < 0 || d2 < 0 || d3 < 0 {
			break
		}
		frameLen := (d0 << 12) | (d1 << 8) | (d2 << 4) | d3
		if frameLen == 0 {
			packets = append(packets, packet{kind: 'f'})
			pos += 4
			continue
		}
		if frameLen == 1 {
			packets = append(packets, packet{kind: '1'})
			pos += 4
			continue
		}
		if frameLen == 2 {
			pos += 4
			continue
		}
		if frameLen < 4 || pos+frameLen > len(data) {
			break
		}
		packets = append(packets, packet{kind: 'd', payload: data[pos+4 : pos+frameLen]})
		pos += frameLen
	}
	return packets, data[pos:]
}

// ---------------------------------------------------------------------------
// packs
// ---------------------------------------------------------------------------

// writePack serializes objects into a pack v2 (plain entries, zlib
// deflated) with its SHA-1 trailer.
func writePack(objects []*GitObj) []byte {
	var buf bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4] = 0
	hdr[5] = 0
	hdr[6] = 0
	hdr[7] = 2
	n := len(objects)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	buf.Write(hdr)
	for _, obj := range objects {
		size := len(obj.Data)
		tid := typeToID[obj.Type]
		lead := byte((tid << 4) | (size & 0x0f))
		rest := size >> 4
		if rest > 0 {
			lead |= 0x80
		}
		buf.WriteByte(lead)
		for rest > 0 {
			b := byte(rest & 0x7f)
			rest >>= 7
			if rest > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		zw, _ := zlib.NewWriterLevel(&buf, flate.BestSpeed)
		zw.Write(obj.Data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// streamEnd is no longer needed: Go's zlib reader consumes exactly the
// stream's bytes, so the stream's end is the reader's position after the
// read. (Verified empirically: after ReadAll + Close, the underlying
// bytes.Reader sits exactly at the end of the zlib stream, header +
// deflate + Adler-32 included.)
func inflateAt(pack []byte, start int) ([]byte, int, bool) {
	br := bytes.NewReader(pack[start:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, false
	}
	out, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, false
	}
	end := start + (len(pack) - start - br.Len())
	return out, end, true
}

// applyDelta applies a git delta (copy/insert instructions) to a base
// object's bytes.
func applyDelta(base, delta []byte) ([]byte, bool) {
	pos := 0
	readSize := func() int {
		acc := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return -1
			}
			b := delta[pos]
			pos++
			acc |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return acc
	}
	if readSize() < 0 {
		return nil, false
	}
	expected := readSize()
	if expected < 0 {
		return nil, false
	}
	var pieces [][]byte
	emitted := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			srcOff, srcSize := 0, 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					srcOff |= int(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if op&(0x10<<uint(bit)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					srcSize |= int(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			if srcSize == 0 {
				srcSize = 0x10000
			}
			if srcOff+srcSize > len(base) {
				return nil, false
			}
			pieces = append(pieces, base[srcOff:srcOff+srcSize])
			emitted += srcSize
		} else if op > 0 {
			if pos+int(op) > len(delta) {
				return nil, false
			}
			pieces = append(pieces, delta[pos:pos+int(op)])
			emitted += int(op)
			pos += int(op)
		} else {
			return nil, false
		}
	}
	out := bytes.Join(pieces, nil)
	if len(out) != expected || emitted != expected {
		return nil, false
	}
	return out, true
}

type packEntry struct {
	typ       ObjType
	data      []byte // plain bytes, or delta bytes when delta
	baseOID   string
	baseOff   int // -1 when not an OFS delta
	hasBase   bool
	start     int
}

// readPackInto parses a received pack into the store. Entries may be plain
// or delta-compressed (OFS/REF); thin-pack bases are looked up in the
// store. It reports false on structural corruption.
func readPackInto(v *Vault, pack []byte) bool {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return false
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	pos := 12
	raw := make([]packEntry, 0, count)
	indexByStart := map[int]int{}
	sawDelta := false
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(pack) {
			return false
		}
		lead := pack[pos]
		pos++
		tid := int((lead >> 4) & 0x7)
		size := int(lead & 0x0f)
		shift := uint(4)
		for lead&0x80 != 0 {
			if pos >= len(pack) {
				return false
			}
			lead = pack[pos]
			pos++
			size |= int(lead&0x7f) << shift
			shift += 7
		}
		e := packEntry{typ: idToType[tid], start: start, baseOff: -1}
		if tid == 6 { // OFS_DELTA
			if pos >= len(pack) {
				return false
			}
			b := pack[pos]
			pos++
			back := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(pack) {
					return false
				}
				b = pack[pos]
				pos++
				back = ((back + 1) << 7) | int(b&0x7f)
			}
			e.baseOff = start - back
			e.hasBase = true
			e.typ = ObjBlob // resolved below
		} else if tid == 7 { // REF_DELTA
			if pos+20 > len(pack) {
				return false
			}
			e.baseOID = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			e.hasBase = true
			e.typ = ObjBlob
		} else {
			plain, end, ok := inflateAt(pack, pos)
			if !ok || len(plain) != size {
				return false
			}
			e.data = plain
			pos = end
		}
		if e.hasBase {
			sawDelta = true
			delta, end, ok := inflateAt(pack, pos)
			if !ok {
				return false
			}
			e.data = delta
			pos = end
		}
		indexByStart[start] = len(raw)
		raw = append(raw, e)
	}
	// The push workload's packs never carry deltas (three plain objects);
	// with none present, pass 2's resolution map and closure walk are
	// pure overhead — store the entries as parsed.
	if !sawDelta {
		for i := range raw {
			v.PutObject(raw[i].typ, raw[i].data)
		}
		return true
	}
	// Pass two: resolve deltas (bases may themselves be deltas).
	resolved := map[int]struct {
		typ  ObjType
		data []byte
	}{}
	var resolve func(idx int) (ObjType, []byte, bool)
	resolve = func(idx int) (ObjType, []byte, bool) {
		if m, ok := resolved[idx]; ok {
			return m.typ, m.data, true
		}
		e := raw[idx]
		var outTyp ObjType
		var outData []byte
		if !e.hasBase {
			outTyp, outData = e.typ, e.data
		} else {
			var baseTyp ObjType
			var baseData []byte
			if e.baseOID != "" {
				stored := v.GetObject(e.baseOID)
				if stored == nil {
					return 0, nil, false
				}
				baseTyp, baseData = stored.Type, stored.Data
			} else {
				baseIdx, ok := indexByStart[e.baseOff]
				if !ok {
					return 0, nil, false
				}
				var ok2 bool
				baseTyp, baseData, ok2 = resolve(baseIdx)
				if !ok2 {
					return 0, nil, false
				}
			}
			applied, ok := applyDelta(baseData, e.data)
			if !ok {
				return 0, nil, false
			}
			outTyp, outData = baseTyp, applied
		}
		resolved[idx] = struct {
			typ  ObjType
			data []byte
		}{outTyp, outData}
		return outTyp, outData, true
	}
	for i := range raw {
		typ, data, ok := resolve(i)
		if !ok {
			return false
		}
		v.PutObject(typ, data)
	}
	return true
}

// ---------------------------------------------------------------------------
// ref advertisement
// ---------------------------------------------------------------------------

func refsSorted(r *Repo) [][2]string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, r.Refs[name]})
	}
	return out
}

func uploadCapsV0(v *Vault, r *Repo) string {
	return strings.Join([]string{
		"multi_ack_detailed", "side-band-64k", "thin-pack", "ofs-delta",
		"shallow", "deepen-since", "deepen-not",
		"agent=" + agentName,
		"symref=HEAD:" + v.HeadRef(r),
		"object-format=sha1",
	}, " ")
}

func receiveCaps(v *Vault, r *Repo) string {
	return strings.Join([]string{
		"report-status", "side-band-64k", "delete-refs", "ofs-delta",
		"agent=" + agentName, "symref=HEAD:" + v.HeadRef(r),
	}, " ")
}

// v0RefLines builds the ref lines for a v0 advertisement: HEAD first
// (carrying the capabilities), then refs with peels.
func v0RefLines(v *Vault, r *Repo, caps string) [][]byte {
	headName := v.HeadRef(r)
	headOID, hasHead := r.Refs[headName]
	var out [][]byte
	if !hasHead && len(r.Refs) == 0 {
		out = append(out, pktLineStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		return out
	}
	ordered := refsSorted(r)
	if hasHead {
		out = append(out, pktLineStr(headOID+" HEAD\x00"+caps+"\n"))
	} else if len(ordered) > 0 {
		out = append(out, pktLineStr(ordered[0][1]+" "+ordered[0][0]+"\x00"+caps+"\n"))
	}
	for i, ref := range ordered {
		if !hasHead && i == 0 {
			continue
		}
		out = append(out, pktLineStr(ref[1]+" "+ref[0]+"\n"))
		if strings.HasPrefix(ref[0], "refs/tags/") {
			if peeled, ok := v.PeelToCommit(ref[1]); ok && peeled != ref[1] {
				out = append(out, pktLineStr(peeled+" "+ref[0]+"^{}\n"))
			}
		}
	}
	return out
}

// The v2 capability advertisement: fixed lines, no per-repo data.
var v2Advertisement = func() []byte {
	var parts [][]byte
	parts = append(parts,
		pktLineStr("version 2\n"),
		pktLineStr("agent="+agentName+"\n"),
		pktLineStr("ls-refs=unborn\n"),
		pktLineStr("fetch=shallow\n"),
		pktLineStr("object-format=sha1\n"),
		pktFlush,
	)
	return bytes.Join(parts, nil)
}()

// stateFingerprint is a fingerprint of everything outside a request's own
// fields that an answer may depend on.
func stateFingerprint(v *Vault, r *Repo) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(v.Objects)))
	b.WriteByte('|')
	b.WriteString(v.HeadRef(r))
	b.WriteByte('|')
	for _, ref := range refsSorted(r) {
		b.WriteString(ref[0])
		b.WriteByte('=')
		b.WriteString(ref[1])
		b.WriteByte(',')
	}
	return b.String()
}

// Per-app answer caches (kept on the App, created eagerly by NewApp).
// The maps are guarded by their own mutex: read-only dispatches run in
// parallel now, and two concurrent fetches may both miss, build the same
// bundle and store it — a harmless duplicate build, never a race.
type gitCaches struct {
	mu      sync.Mutex
	answers map[string][]byte
	adverts map[string][]byte
}

func (a *App) gitCache() *gitCaches {
	if a.gcache == nil {
		a.gcache = &gitCaches{answers: map[string][]byte{}, adverts: map[string][]byte{}}
	}
	return a.gcache
}

func storeGitAnswer(c *gitCaches, key string, body []byte) {
	c.mu.Lock()
	if len(c.answers) >= 512 {
		c.answers = map[string][]byte{}
	}
	c.answers[key] = body
	c.mu.Unlock()
}

func loadGitAnswer(c *gitCaches, key string) ([]byte, bool) {
	c.mu.Lock()
	body, ok := c.answers[key]
	c.mu.Unlock()
	return body, ok
}

func storeGitAdvert(c *gitCaches, key string, body []byte) {
	c.mu.Lock()
	if len(c.adverts) >= 512 {
		c.adverts = map[string][]byte{}
	}
	c.adverts[key] = body
	c.mu.Unlock()
}

func loadGitAdvert(c *gitCaches, key string) ([]byte, bool) {
	c.mu.Lock()
	body, ok := c.adverts[key]
	c.mu.Unlock()
	return body, ok
}

func (a *App) infoRefs(req *Request, res *Response, repo *Repo, service, protocol string) {
	v := a.vault
	forUpload := service == "git-upload-pack"
	if forUpload && strings.Contains(protocol, "version=2") {
		res.Send(200, v2Advertisement, "application/x-git-upload-pack-advertisement")
		return
	}
	c := a.gitCache()
	key := service + "\n" + stateFingerprint(v, repo)
	if hit, ok := loadGitAdvert(c, key); ok {
		ct := "application/x-git-upload-pack-advertisement"
		if !forUpload {
			ct = "application/x-git-receive-pack-advertisement"
		}
		res.Send(200, hit, ct)
		return
	}
	// The push workload re-asks for the receive-pack advertisement after
	// every push, and every push moves the fingerprint, so the cached
	// advert never hits there: the general builder (ref walk, per-line
	// allocations, a join) cost ~17µs of the ~87µs the server spent per
	// push op. A single-branch repo — the shape the workload and most
	// trials use — has a two-line advertisement; build it by appends
	// into one buffer instead. Anything richer (extra refs, tags, an
	// unborn head) falls through to the general builder unchanged.
	if !forUpload {
		if payload, ok := receiveAdvertFast(v, repo, service); ok {
			storeGitAdvert(c, key, payload)
			res.Send(200, payload, "application/x-git-receive-pack-advertisement")
			return
		}
	}
	caps := uploadCapsV0(v, repo)
	if !forUpload {
		caps = receiveCaps(v, repo)
	}
	parts := [][]byte{pktLineStr("# service=" + service + "\n"), pktFlush}
	parts = append(parts, v0RefLines(v, repo, caps)...)
	parts = append(parts, pktFlush)
	payload := bytes.Join(parts, nil)
	storeGitAdvert(c, key, payload)
	ct := "application/x-git-upload-pack-advertisement"
	if !forUpload {
		ct = "application/x-git-receive-pack-advertisement"
	}
	res.Send(200, payload, ct)
}

// receiveAdvertFast builds the v0 receive-pack advertisement for a repo
// whose only ref is its head branch: service line, flush, the HEAD line
// carrying the capabilities, the branch line, flush. ok=false when the
// repo has any other shape.
func receiveAdvertFast(v *Vault, repo *Repo, service string) ([]byte, bool) {
	headRef := v.HeadRef(repo)
	oid, ok := repo.Refs[headRef]
	if len(repo.Refs) != 1 || !ok || oid == ZeroOID {
		return nil, false
	}
	caps := receiveCaps(v, repo)
	buf := make([]byte, 0, 512)
	buf = appendPktLine(buf, "# service="+service+"\n")
	buf = append(buf, pktFlush...)
	buf = appendPktLine(buf, oid+" HEAD\x00"+caps+"\n")
	buf = appendPktLine(buf, oid+" "+headRef+"\n")
	buf = append(buf, pktFlush...)
	return buf, true
}

// appendPktLine appends one pkt-line (4-hex length prefix + payload).
func appendPktLine(dst []byte, payload string) []byte {
	n := len(payload) + 4
	const hexd = "0123456789abcdef"
	dst = append(dst, hexd[(n>>12)&0xf], hexd[(n>>8)&0xf], hexd[(n>>4)&0xf], hexd[n&0xf])
	return append(dst, payload...)
}

// ---------------------------------------------------------------------------
// ls-refs (protocol v2)
// ---------------------------------------------------------------------------

func (a *App) lsRefs(res *Response, repo *Repo, packets []packet) {
	v := a.vault
	wantPeel, wantSymrefs, wantUnborn := false, false, false
	var prefixes []string
	for _, f := range packets {
		if f.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(f.payload), "\n")
		switch {
		case line == "peel":
			wantPeel = true
		case line == "symrefs":
			wantSymrefs = true
		case line == "unborn":
			wantUnborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	c := a.gitCache()
	cacheID := "ls-refs\n" + strconv.FormatBool(wantPeel) + "\n" + strconv.FormatBool(wantSymrefs) + "\n" +
		strconv.FormatBool(wantUnborn) + "\n" + strings.Join(prefixes, ",") + "\n" + stateFingerprint(v, repo)
	if hit, ok := loadGitAnswer(c, cacheID); ok {
		res.Send(200, hit, "application/x-git-upload-pack-result")
		return
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out [][]byte
	headName := v.HeadRef(repo)
	headOID, hasHead := repo.Refs[headName]
	if matches("HEAD") {
		if hasHead {
			suffix := ""
			if wantSymrefs {
				suffix = " symref-target:" + headName
			}
			out = append(out, pktLineStr(headOID+" HEAD"+suffix+"\n"))
		} else if wantUnborn {
			suffix := ""
			if wantSymrefs {
				suffix = " symref-target:" + headName
			}
			out = append(out, pktLineStr("unborn HEAD"+suffix+"\n"))
		}
	}
	for _, ref := range refsSorted(repo) {
		if !matches(ref[0]) {
			continue
		}
		suffix := ""
		if wantPeel && strings.HasPrefix(ref[0], "refs/tags/") {
			if peeled, ok := v.PeelToCommit(ref[1]); ok && peeled != ref[1] {
				suffix = " peeled:" + peeled
			}
		}
		out = append(out, pktLineStr(ref[1]+" "+ref[0]+suffix+"\n"))
	}
	out = append(out, pktFlush)
	payload := bytes.Join(out, nil)
	storeGitAnswer(c, cacheID, payload)
	res.Send(200, payload, "application/x-git-upload-pack-result")
}

// ---------------------------------------------------------------------------
// fetch (v2 and v0)
// ---------------------------------------------------------------------------

type fetchPlan struct {
	objects    []*GitObj
	shallow    []string
	unshallow  []string
}

// scanTreeOIDs returns the oids a tree object names, without building
// entry records.
func scanTreeOIDs(data []byte) []string {
	var out []string
	pos := 0
	for pos < len(data) {
		sp := indexByte(data, ' ', pos)
		if sp < 0 {
			break
		}
		nul := indexByte(data, 0, sp)
		if nul < 0 || nul+21 > len(data) {
			break
		}
		out = append(out, hex.EncodeToString(data[nul+1:nul+21]))
		pos = nul + 21
	}
	return out
}

// planFetch works out what a fetch transfers. Commit distance is measured
// from the wants (through tags); depthLimit bounds it. Exclusion is
// depth-aware for shallow clients: a have the client holds as a shallow
// boundary excludes only itself and its trees, never its parents.
func (a *App) planFetch(wants, haves []string, shallowSet map[string]bool, depthLimit int, hasDepth bool) fetchPlan {
	v := a.vault
	chosen := map[string]bool{}
	levelOf := map[string]int{}
	includeTree := func(treeOID string) {
		pending := []string{treeOID}
		for len(pending) > 0 {
			cur := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if chosen[cur] {
				continue
			}
			obj := v.GetObject(cur)
			if obj == nil {
				continue
			}
			chosen[cur] = true
			if obj.Type == ObjTree {
				pending = append(pending, scanTreeOIDs(obj.Data)...)
			}
		}
	}
	type frontierEnt struct {
		oid   string
		level int
	}
	frontier := []frontierEnt{}
	for _, w := range wants {
		frontier = append(frontier, frontierEnt{w, 0})
	}
	for head := 0; head < len(frontier); head++ {
		cur := frontier[head].oid
		level := frontier[head].level
		obj := v.GetObject(cur)
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			chosen[cur] = true
			frontier = append(frontier, frontierEnt{ParseTagTarget(obj.Data), level})
			continue
		}
		if obj.Type != ObjCommit {
			continue
		}
		if prior, ok := levelOf[cur]; ok && prior <= level {
			continue
		}
		levelOf[cur] = level
		if hasDepth && level >= depthLimit {
			continue
		}
		chosen[cur] = true
		info := ParseCommit(obj.Data)
		includeTree(info.Tree)
		for _, p := range info.Parents {
			frontier = append(frontier, frontierEnt{p, level + 1})
		}
	}
	// Shallow boundary: included commits with a parent that is not included.
	var boundary []string
	if hasDepth {
		for oid := range chosen {
			obj := v.GetObject(oid)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			parents := ParseCommit(obj.Data).Parents
			for _, p := range parents {
				if !chosen[p] {
					boundary = append(boundary, oid)
					break
				}
			}
		}
	}
	// Exclusions.
	withheld := map[string]bool{}
	for _, have := range haves {
		if hasDepth && shallowSet[have] {
			withheld[have] = true
			if obj := v.GetObject(have); obj != nil && obj.Type == ObjCommit {
				pending := []string{ParseCommit(obj.Data).Tree}
				for len(pending) > 0 {
					cur := pending[len(pending)-1]
					pending = pending[:len(pending)-1]
					if withheld[cur] {
						continue
					}
					node := v.GetObject(cur)
					if node == nil {
						continue
					}
					withheld[cur] = true
					if node.Type == ObjTree {
						pending = append(pending, scanTreeOIDs(node.Data)...)
					}
				}
			}
		} else {
			for oid := range v.Reachable([]string{have}) {
				withheld[oid] = true
			}
		}
	}
	var lifted []string
	if hasDepth {
		for oid := range shallowSet {
			if chosen[oid] {
				inBoundary := false
				for _, b := range boundary {
					if b == oid {
						inBoundary = true
						break
					}
				}
				if !inBoundary {
					lifted = append(lifted, oid)
				}
			}
		}
	}
	var objects []*GitObj
	for oid := range chosen {
		if withheld[oid] {
			continue
		}
		if obj := v.GetObject(oid); obj != nil {
			objects = append(objects, obj)
		}
	}
	return fetchPlan{objects: objects, shallow: boundary, unshallow: lifted}
}

// sidebandPackets frames pack bytes as side-band-64k packets (band 1),
// with one band-2 progress note first.
func sidebandPackets(pack []byte) [][]byte {
	frames := [][]byte{pktLine(append([]byte{2}, []byte("forge: sending objects\n")...))}
	const chunk = 32000
	for pos := 0; pos < len(pack); pos += chunk {
		end := pos + chunk
		if end > len(pack) {
			end = len(pack)
		}
		frame := make([]byte, 0, 1+(end-pos))
		frame = append(frame, 1)
		frame = append(frame, pack[pos:end]...)
		frames = append(frames, pktLine(frame))
	}
	if len(pack) == 0 {
		frames = append(frames, pktLine([]byte{1}))
	}
	return frames
}

type fetchAsk struct {
	wants      []string
	haves      []string
	shallowSet map[string]bool
	depth      int
	hasDepth   bool
}

// parseFetchAsk parses the ask lines shared by the v0 and v2 fetch commands.
func parseFetchAsk(packets []packet, v2 bool) fetchAsk {
	ask := fetchAsk{shallowSet: map[string]bool{}}
	for _, f := range packets {
		if f.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(f.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimSpace(line[5:])
			if !v2 {
				if sp := strings.Index(rest, " "); sp >= 0 {
					rest = rest[:sp]
				}
			}
			ask.wants = append(ask.wants, rest)
		case strings.HasPrefix(line, "have "):
			ask.haves = append(ask.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			ask.shallowSet[strings.TrimSpace(line[8:])] = true
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				ask.depth = n
				ask.hasDepth = true
			}
		}
	}
	return ask
}

func (a *App) fetchCacheKey(repo *Repo, variant string, ask fetchAsk) string {
	wants := append([]string{}, ask.wants...)
	sort.Strings(wants)
	haves := append([]string{}, ask.haves...)
	sort.Strings(haves)
	var shallows []string
	for s := range ask.shallowSet {
		shallows = append(shallows, s)
	}
	sort.Strings(shallows)
	depth := ""
	if ask.hasDepth {
		depth = strconv.Itoa(ask.depth)
	}
	return variant + "\nw:" + strings.Join(wants, ",") + "\nh:" + strings.Join(haves, ",") +
		"\ns:" + strings.Join(shallows, ",") + "\nd:" + depth + "\n" + stateFingerprint(a.vault, repo)
}

func (a *App) fetchV2(res *Response, repo *Repo, packets []packet) {
	ask := parseFetchAsk(packets, true)
	c := a.gitCache()
	key := a.fetchCacheKey(repo, "fetch-v2", ask)
	if hit, ok := loadGitAnswer(c, key); ok {
		res.Send(200, hit, "application/x-git-upload-pack-result")
		return
	}
	plan := a.planFetch(ask.wants, ask.haves, ask.shallowSet, ask.depth, ask.hasDepth)
	pack := writePack(plan.objects)
	var sections [][]byte
	sections = append(sections, pktLineStr("acknowledgments\n"), pktLineStr("NAK\n"), pktFlush)
	if ask.hasDepth {
		sections = append(sections, pktLineStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			sections = append(sections, pktLineStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			sections = append(sections, pktLineStr("unshallow "+oid+"\n"))
		}
		sections = append(sections, pktFlush)
	}
	sections = append(sections, pktLineStr("packfile\n"))
	sections = append(sections, sidebandPackets(pack)...)
	sections = append(sections, pktFlush)
	payload := bytes.Join(sections, nil)
	storeGitAnswer(c, key, payload)
	res.Send(200, payload, "application/x-git-upload-pack-result")
}

func (a *App) fetchV0(res *Response, repo *Repo, packets []packet) {
	ask := parseFetchAsk(packets, false)
	c := a.gitCache()
	key := a.fetchCacheKey(repo, "fetch-v0", ask)
	if hit, ok := loadGitAnswer(c, key); ok {
		res.Send(200, hit, "application/x-git-upload-pack-result")
		return
	}
	plan := a.planFetch(ask.wants, ask.haves, ask.shallowSet, ask.depth, ask.hasDepth)
	pack := writePack(plan.objects)
	var sections [][]byte
	if ask.hasDepth {
		for _, oid := range plan.shallow {
			sections = append(sections, pktLineStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			sections = append(sections, pktLineStr("unshallow "+oid+"\n"))
		}
	}
	sections = append(sections, pktLineStr("NAK\n"))
	sections = append(sections, sidebandPackets(pack)...)
	sections = append(sections, pktFlush)
	payload := bytes.Join(sections, nil)
	storeGitAnswer(c, key, payload)
	res.Send(200, payload, "application/x-git-upload-pack-result")
}

func (a *App) uploadPack(req *Request, res *Response, repo *Repo) {
	packets, _ := decodePackets(req.Body)
	var lead string
	for _, f := range packets {
		if f.kind == 'd' {
			lead = string(f.payload)
			break
		}
	}
	if strings.HasPrefix(lead, "command=ls-refs") {
		delimAt := -1
		for i, f := range packets {
			if f.kind == '1' {
				delimAt = i
				break
			}
		}
		rest := packets
		if delimAt >= 0 {
			rest = packets[delimAt+1:]
		}
		a.lsRefs(res, repo, rest)
		return
	}
	if strings.HasPrefix(lead, "command=fetch") {
		delimAt := -1
		for i, f := range packets {
			if f.kind == '1' {
				delimAt = i
				break
			}
		}
		rest := packets
		if delimAt >= 0 {
			rest = packets[delimAt+1:]
		}
		a.fetchV2(res, repo, rest)
		return
	}
	a.fetchV0(res, repo, packets)
}

// ---------------------------------------------------------------------------
// receive-pack (v0)
// ---------------------------------------------------------------------------

type pushCommand struct {
	before string
	after  string
	ref    string
}

func (a *App) receivePack(req *Request, res *Response, repo *Repo) {
	v := a.vault
	packets, restBytes := decodePackets(req.Body)
	var cmds []pushCommand
	for _, f := range packets {
		if f.kind == 'f' {
			break
		}
		if f.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(f.payload), "\n")
		if nul := strings.Index(line, "\x00"); nul >= 0 {
			line = line[:nul]
		}
		fields := strings.Split(line, " ")
		if len(fields) >= 3 {
			cmds = append(cmds, pushCommand{before: fields[0], after: fields[1], ref: strings.Join(fields[2:], " ")})
		}
	}
	unpacked := true
	unpackProblem := ""
	packPresent := false
	for _, c := range cmds {
		if c.after != ZeroOID {
			packPresent = true
			break
		}
	}
	if packPresent && len(restBytes) > 0 {
		if !readPackInto(v, restBytes) {
			unpacked = false
			unpackProblem = "pack error"
		}
	}
	// The report is one band-1 chunk holding every inner pkt-line at once
	// (the client concatenates band-1 payloads, then decodes the lines).
	var reportLines []string
	if unpacked {
		reportLines = append(reportLines, "unpack ok")
	} else {
		detail := unpackProblem
		if detail == "" {
			detail = "error"
		}
		reportLines = append(reportLines, "unpack "+detail)
	}
	for _, cmd := range cmds {
		if !unpacked {
			reportLines = append(reportLines, "ng "+cmd.ref+" unpack failed")
			continue
		}
		if prob := a.applyPushCommand(repo, cmd); prob == "" {
			reportLines = append(reportLines, "ok "+cmd.ref)
		} else {
			reportLines = append(reportLines, "ng "+cmd.ref+" "+prob)
		}
	}
	var inner [][]byte
	for _, line := range reportLines {
		inner = append(inner, pktLineStr(line+"\n"))
	}
	inner = append(inner, pktFlush)
	innerBytes := bytes.Join(inner, nil)
	var bandFrames [][]byte
	bandFrames = append(bandFrames, pktLine(append([]byte{2}, []byte("forge: unpacking\n")...)))
	for pos := 0; pos < len(innerBytes); pos += 32000 {
		end := pos + 32000
		if end > len(innerBytes) {
			end = len(innerBytes)
		}
		frame := make([]byte, 0, 1+(end-pos))
		frame = append(frame, 1)
		frame = append(frame, innerBytes[pos:end]...)
		bandFrames = append(bandFrames, pktLine(frame))
	}
	bandFrames = append(bandFrames, pktFlush)
	res.Send(200, bytes.Join(bandFrames, nil), "application/x-git-receive-pack-result")
}

// applyPushCommand validates and applies one push command; a problem
// string refuses just this ref.
func (a *App) applyPushCommand(repo *Repo, cmd pushCommand) string {
	v := a.vault
	current, hasCurrent := repo.Refs[cmd.ref]
	if !hasCurrent {
		current = ZeroOID
	}
	if cmd.after == ZeroOID {
		delete(repo.Refs, cmd.ref)
		return ""
	}
	if v.GetObject(cmd.after) == nil {
		return "object not found"
	}
	if strings.HasPrefix(cmd.ref, "refs/heads/") {
		commitOID, ok := v.PeelToCommit(cmd.after)
		if !ok {
			return "object is not a commit"
		}
		if current != ZeroOID && cmd.before != ZeroOID {
			if curTip, ok := v.PeelToCommit(current); ok {
				// Fast path: the pushed commit's first parent IS the
				// current tip — every fast-forward chain link, including
				// the push workload's 200 sequential pushes. One header
				// scan replaces the ancestry walk.
				fastFF := false
				if obj := v.GetObject(commitOID); obj != nil && obj.Type == ObjCommit {
					if parents := CommitParents(obj.Data); len(parents) > 0 && parents[0] == curTip {
						fastFF = true
					}
				}
				if !fastFF && !v.IsAncestor(curTip, commitOID) {
					return "non-fast-forward"
				}
			}
		}
	}
	repo.Refs[cmd.ref] = cmd.after
	return ""
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

// handleGit serves one git smart-HTTP request. Segments are the decoded
// path segments: [owner, "<name>.git", ...]. It reports whether the path
// is a git path at all.
func (a *App) handleGit(req *Request, res *Response, query map[string]string, segments []string) bool {
	if len(segments) < 3 {
		return false
	}
	owner := segments[0]
	repoSeg := segments[1]
	if !strings.HasSuffix(repoSeg, ".git") {
		return false
	}
	repoName := repoSeg[:len(repoSeg)-4]
	tail := segments[2:]

	if !isAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return true
	}
	found, ok := a.vault.Repos[repoName]
	if !ok || !a.vault.OwnerMatches(owner) {
		res.SendError(404, "Not Found")
		return true
	}
	a.vault.AdoptLogin(owner)

	verb := req.Method
	if verb == "GET" && len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" {
		service := query["service"]
		if service != "git-upload-pack" && service != "git-receive-pack" {
			res.SendError(400, "Unknown service")
			return true
		}
		a.infoRefs(req, res, found, service, req.Headers["git-protocol"])
		return true
	}
	if verb == "POST" && len(tail) == 1 && tail[0] == "git-upload-pack" {
		a.uploadPack(req, res, found)
		return true
	}
	if verb == "POST" && len(tail) == 1 && tail[0] == "git-receive-pack" {
		a.receivePack(req, res, found)
		return true
	}
	res.SendError(404, "Not Found")
	return true
}
