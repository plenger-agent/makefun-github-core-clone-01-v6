// The forge's in-memory vault: the git object store (SHA-1 addressed
// blobs, trees, commits and tags), the repositories built on it, and the
// issue / pull-request domain the REST, GraphQL and MCP surfaces share.
// One vault serves every surface, so an object created through any of
// them is the same object through the others.
//
// Nothing here reads the wall clock or a random source: timestamps come
// from a monotonic sequence laid over a fixed base epoch, ids from
// counters, and request ids from a counter too. State is therefore a pure
// function of the request sequence, which is what a trial's live pass and
// its replay both need.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ObjType int

const (
	ObjCommit ObjType = 1
	ObjTree   ObjType = 2
	ObjBlob   ObjType = 3
	ObjTag    ObjType = 4
)

func (t ObjType) Name() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	default:
		return "tag"
	}
}

type GitObj struct {
	Type ObjType
	Data []byte
}

const ZeroOID = "0000000000000000000000000000000000000000"

const baseEpochSec = 1700000000

// HashObject returns the SHA-1 of a git object: "<type> <len>\0" + bytes.
func HashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ.Name() + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree splits a tree object's raw bytes into entries, in stored order.
func ParseTree(data []byte) []TreeEntry {
	var rows []TreeEntry
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
		rows = append(rows, TreeEntry{
			Mode: string(data[pos:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		pos = nul + 21
	}
	return rows
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" || e.Mode == "040000" {
		return e.Name + "/"
	}
	return e.Name
}

// BuildTree serializes tree entries into a tree object's raw bytes,
// sorted the way git sorts them.
func BuildTree(entries []TreeEntry) []byte {
	ordered := make([]TreeEntry, len(entries))
	copy(ordered, entries)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && treeSortKey(ordered[j]) < treeSortKey(ordered[j-1]); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	var out []byte
	for _, e := range ordered {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit reads a commit object's headers and message. Only the header
// block is turned into text; the message is sliced off the raw bytes.
// CommitParents scans only the parent lines of a commit header — the
// cheap shape test the push fast-forward fast path needs, without
// ParseCommit's full ident/message decoding.
func CommitParents(data []byte) []string {
	var parents []string
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		if len(line) == 0 {
			break // blank line: header over
		}
		if bytes.HasPrefix(line, []byte("parent ")) && len(line) == 47 {
			parents = append(parents, string(line[7:]))
		}
	}
	return parents
}

func ParseCommit(data []byte) CommitInfo {
	var info CommitInfo
	split := -1
	for i := 0; i+1 < len(data); i++ {
		if data[i] == '\n' && data[i+1] == '\n' {
			split = i
			break
		}
	}
	var header, body string
	if split == -1 {
		header = string(data)
	} else {
		header = string(data[:split])
		body = string(data[split+2:])
	}
	info.Message = body
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			info.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			info.Committer = line[10:]
		}
	}
	return info
}

// ParseTagTarget returns the `object` header of an annotated tag.
func ParseTagTarget(data []byte) string {
	text := string(data)
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

type identParts struct {
	name  string
	email string
	epoch int64
}

// ParseIdent splits "Name <email> 1700000000 +0000" into its parts.
func ParseIdent(line string) identParts {
	line = strings.TrimSpace(line)
	lt := strings.Index(line, "<")
	gt := strings.Index(line, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return identParts{name: line, epoch: baseEpochSec}
	}
	name := strings.TrimSpace(line[:lt])
	email := line[lt+1 : gt]
	rest := strings.Fields(line[gt+1:])
	var epoch int64 = baseEpochSec
	if len(rest) > 0 {
		if n, err := strconv.ParseInt(rest[0], 10, 64); err == nil {
			epoch = n
		}
	}
	return identParts{name: name, email: email, epoch: epoch}
}

// ISOAt renders epoch seconds as an ISO-8601 timestamp, byte-identical
// to Date.toISOString for whole seconds ("...T...:....000Z").
func ISOAt(epochSec int64) string {
	return time.Unix(epochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// ---------------------------------------------------------------------------
// Domain records
// ---------------------------------------------------------------------------

type CommentRec struct {
	ID   int
	Body string
	Seq  int64
}

type IssueRec struct {
	Number   int
	ID       int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*CommentRec
	Seq      int64
}

type PullRec struct {
	Number         int
	ID             int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	MergeCommitSHA string
	MergedSeq      int64
	Comments       []*CommentRec
	Seq            int64
}

type LabelRec struct {
	Name  string
	Color string
}

type Repo struct {
	ID            int
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string // full ref name -> commit oid
	Labels        map[string]*LabelRec
	Issues        map[int]*IssueRec
	Pulls         map[int]*PullRec
	NextNumber    int
	Seq           int64
}

// PathHit is what a path inside a tree resolves to.
type PathHit struct {
	Kind    string // "file" | "dir"
	OID     string
	Data    []byte
	Mode    string
	Entries []TreeEntry
}

// ---------------------------------------------------------------------------
// Vault
// ---------------------------------------------------------------------------

type Vault struct {
	Objects      map[string]*GitObj
	Repos        map[string]*Repo
	Login        string
	HasLogin     bool
	loginMu      sync.RWMutex
	tick         int64
	nextRepoID   int
	nextIssueID  int
	nextCmtID    int
	nextReqNo    int64
}

func NewVault() *Vault {
	return &Vault{
		Objects: map[string]*GitObj{},
		Repos:   map[string]*Repo{},
	}
}

// Seq returns the next deterministic sequence number. Atomic so read-only
// dispatches running in parallel can never tear the counter; the values
// handed out under parallel dispatch may interleave differently than a
// serial run would, but every value is still unique and monotonic.
func (v *Vault) Seq() int64 {
	return atomic.AddInt64(&v.tick, 1)
}

func (v *Vault) ISOAtSeq(seq int64) string { return ISOAt(baseEpochSec + seq) }
func (v *Vault) EpochAt(seq int64) int64   { return baseEpochSec + seq }

// NextRequestID mints a fixed-width id ("FORGE-" + 6 base-36 digits) so a
// memoised response head can be patched with a fresh id in place.
func (v *Vault) NextRequestID() string {
	n := int(atomic.AddInt64(&v.nextReqNo, 1))
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var rev [6]byte
	for i := 5; i >= 0; i-- {
		rev[i] = digits[n%36]
		n /= 36
	}
	return "FORGE-" + string(rev[:])
}

// AdoptLogin learns the token owner's login from the first owner-bearing
// request; after that the login is fixed for the life of the process.
func (v *Vault) AdoptLogin(owner string) {
	if owner == "" {
		return
	}
	v.loginMu.Lock()
	if !v.HasLogin {
		v.Login = owner
		v.HasLogin = true
	}
	v.loginMu.Unlock()
}

// OwnerMatches reports whether `owner` is (or could still become) the
// token owner's login.
func (v *Vault) OwnerMatches(owner string) bool {
	v.loginMu.RLock()
	defer v.loginMu.RUnlock()
	return !v.HasLogin || v.Login == owner
}

// LoginName returns the adopted login ("" before adoption).
func (v *Vault) LoginName() string {
	v.loginMu.RLock()
	defer v.loginMu.RUnlock()
	return v.Login
}

// ---------------------------------------------------------------------------
// Object store
// ---------------------------------------------------------------------------

func (v *Vault) PutObject(typ ObjType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := v.Objects[oid]; !ok {
		v.Objects[oid] = &GitObj{Type: typ, Data: data}
	}
	return oid
}

func (v *Vault) GetObject(oid string) *GitObj { return v.Objects[oid] }

// PeelToCommit follows tag objects to the commit they ultimately name.
func (v *Vault) PeelToCommit(oid string) (string, bool) {
	cur := oid
	for i := 0; i < 16; i++ {
		obj := v.Objects[cur]
		if obj == nil {
			return "", false
		}
		switch obj.Type {
		case ObjCommit:
			return cur, true
		case ObjTag:
			cur = ParseTagTarget(obj.Data)
		default:
			return "", false
		}
	}
	return "", false
}

// Reachable returns every object reachable from the given tips.
func (v *Vault) Reachable(tips []string) map[string]bool {
	seen := map[string]bool{}
	pending := append([]string{}, tips...)
	for len(pending) > 0 {
		cur := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[cur] {
			continue
		}
		obj := v.Objects[cur]
		if obj == nil {
			continue
		}
		seen[cur] = true
		switch obj.Type {
		case ObjCommit:
			info := ParseCommit(obj.Data)
			pending = append(pending, info.Tree)
			pending = append(pending, info.Parents...)
		case ObjTree:
			for _, e := range ParseTree(obj.Data) {
				pending = append(pending, e.OID)
			}
		case ObjTag:
			pending = append(pending, ParseTagTarget(obj.Data))
		}
	}
	return seen
}

// IsAncestor reports whether ancestor is descendant itself or one of its
// ancestors.
func (v *Vault) IsAncestor(ancestor, descendant string) bool {
	seen := map[string]bool{}
	pending := []string{descendant}
	for len(pending) > 0 {
		cur := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if obj := v.Objects[cur]; obj != nil && obj.Type == ObjCommit {
			pending = append(pending, ParseCommit(obj.Data).Parents...)
		}
	}
	return false
}

// MergeBase finds a best common ancestor of two commits (breadth-first
// from both sides), or "" when there is none.
func (v *Vault) MergeBase(a, b string) string {
	if a == b {
		return a
	}
	depthA := map[string]int{}
	frontier := [][2]any{{a, 0}}
	for head := 0; head < len(frontier); head++ {
		cur := frontier[head][0].(string)
		dist := frontier[head][1].(int)
		if _, ok := depthA[cur]; ok {
			continue
		}
		depthA[cur] = dist
		if obj := v.Objects[cur]; obj != nil && obj.Type == ObjCommit {
			for _, p := range ParseCommit(obj.Data).Parents {
				frontier = append(frontier, [2]any{p, dist + 1})
			}
		}
	}
	seenB := map[string]bool{}
	frontierB := [][2]any{{b, 0}}
	winner := ""
	best := int(^uint(0) >> 1)
	for head := 0; head < len(frontierB); head++ {
		cur := frontierB[head][0].(string)
		dist := frontierB[head][1].(int)
		if seenB[cur] {
			continue
		}
		seenB[cur] = true
		if dA, ok := depthA[cur]; ok && dA+dist < best {
			winner = cur
			best = dA + dist
		}
		if obj := v.Objects[cur]; obj != nil && obj.Type == ObjCommit {
			for _, p := range ParseCommit(obj.Data).Parents {
				frontierB = append(frontierB, [2]any{p, dist + 1})
			}
		}
	}
	return winner
}

// History returns the commits reachable from a tip, newest first
// (depth-first in parent order).
func (v *Vault) History(tip string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(cur string)
	walk = func(cur string) {
		if seen[cur] {
			return
		}
		obj := v.Objects[cur]
		if obj == nil || obj.Type != ObjCommit {
			return
		}
		seen[cur] = true
		out = append(out, cur)
		for _, p := range ParseCommit(obj.Data).Parents {
			walk(p)
		}
	}
	walk(tip)
	return out
}

// ---------------------------------------------------------------------------
// Trees and files
// ---------------------------------------------------------------------------

// TreeFiles flattens a tree (recursively) into a path -> blob map.
func (v *Vault) TreeFiles(treeOID string) map[string]struct {
	OID  string
	Mode string
} {
	flat := map[string]struct {
		OID  string
		Mode string
	}{}
	var walk func(tree, prefix string)
	walk = func(tree, prefix string) {
		obj := v.Objects[tree]
		if obj == nil || obj.Type != ObjTree {
			return
		}
		for _, e := range ParseTree(obj.Data) {
			full := e.Name
			if prefix != "" {
				full = prefix + "/" + e.Name
			}
			if e.Mode == "40000" || e.Mode == "040000" {
				walk(e.OID, full)
			} else {
				flat[full] = struct {
					OID  string
					Mode string
				}{e.OID, e.Mode}
			}
		}
	}
	walk(treeOID, "")
	return flat
}

// CommitFiles returns the flattened file map of a commit's tree.
func (v *Vault) CommitFiles(commitOID string) map[string]struct {
	OID  string
	Mode string
} {
	obj := v.Objects[commitOID]
	if obj == nil || obj.Type != ObjCommit {
		return map[string]struct {
			OID  string
			Mode string
		}{}
	}
	return v.TreeFiles(ParseCommit(obj.Data).Tree)
}

// TreeFromFiles builds (and stores) a tree hierarchy holding exactly the
// given path -> blob map.
func (v *Vault) TreeFromFiles(files map[string]struct {
	OID  string
	Mode string
}) string {
	type node struct {
		files map[string]struct {
			OID  string
			Mode string
		}
		dirs map[string]*node
	}
	newNode := func() *node {
		return &node{
			files: map[string]struct {
				OID  string
				Mode string
			}{},
			dirs: map[string]*node{},
		}
	}
	root := newNode()
	for path, rec := range files {
		parts := strings.Split(path, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			if _, ok := cur.dirs[seg]; !ok {
				cur.dirs[seg] = newNode()
			}
			cur = cur.dirs[seg]
		}
		cur.files[parts[len(parts)-1]] = rec
	}
	var emit func(n *node) string
	emit = func(n *node) string {
		var rows []TreeEntry
		for name, rec := range n.files {
			rows = append(rows, TreeEntry{Mode: rec.Mode, Name: name, OID: rec.OID})
		}
		for name, child := range n.dirs {
			rows = append(rows, TreeEntry{Mode: "40000", Name: name, OID: emit(child)})
		}
		return v.PutObject(ObjTree, BuildTree(rows))
	}
	return emit(root)
}

// PathAt resolves a repo-relative path at a commit; "" names the root.
func (v *Vault) PathAt(commitOID, path string) *PathHit {
	commitObj := v.Objects[commitOID]
	if commitObj == nil || commitObj.Type != ObjCommit {
		return nil
	}
	treeOID := ParseCommit(commitObj.Data).Tree
	if path == "" {
		treeObj := v.Objects[treeOID]
		if treeObj == nil {
			return nil
		}
		return &PathHit{Kind: "dir", Entries: ParseTree(treeObj.Data)}
	}
	parts := strings.Split(path, "/")
	curTree := treeOID
	for i, seg := range parts {
		treeObj := v.Objects[curTree]
		if treeObj == nil || treeObj.Type != ObjTree {
			return nil
		}
		var row *TreeEntry
		for _, e := range ParseTree(treeObj.Data) {
			if e.Name == seg {
				c := e
				row = &c
				break
			}
		}
		if row == nil {
			return nil
		}
		last := i == len(parts)-1
		if !last {
			if row.Mode != "40000" && row.Mode != "040000" {
				return nil
			}
			curTree = row.OID
			continue
		}
		if row.Mode == "40000" || row.Mode == "040000" {
			sub := v.Objects[row.OID]
			if sub == nil {
				return nil
			}
			return &PathHit{Kind: "dir", Entries: ParseTree(sub.Data)}
		}
		blob := v.Objects[row.OID]
		if blob == nil {
			return nil
		}
		return &PathHit{Kind: "file", OID: row.OID, Data: blob.Data, Mode: row.Mode}
	}
	return nil
}

// MakeCommit creates a commit object with the forge's deterministic
// identity line.
func (v *Vault) MakeCommit(treeOID string, parents []string, message string, seq int64) string {
	stamp := v.EpochAt(seq)
	ident := "Forge <forge@example.invalid> " + strconv.FormatInt(stamp, 10) + " +0000"
	var lines []string
	lines = append(lines, "tree "+treeOID)
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+ident, "committer "+ident)
	body := strings.Join(lines, "\n") + "\n\n" + message
	return v.PutObject(ObjCommit, []byte(body))
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

func (v *Vault) CreateRepo(name, description string, private bool) *Repo {
	v.nextRepoID++
	r := &Repo{
		ID:            v.nextRepoID,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Labels:        map[string]*LabelRec{},
		Issues:        map[int]*IssueRec{},
		Pulls:         map[int]*PullRec{},
		NextNumber:    1,
		Seq:           v.Seq(),
	}
	v.Repos[name] = r
	return r
}

// HeadRef is the ref the repository's HEAD names.
func (v *Vault) HeadRef(r *Repo) string { return "refs/heads/" + r.DefaultBranch }

// BranchTip returns the commit a branch name (or full ref) points at.
func (v *Vault) BranchTip(r *Repo, branch string) (string, bool) {
	full := branch
	if !strings.HasPrefix(branch, "refs/") {
		full = "refs/heads/" + branch
	}
	oid, ok := r.Refs[full]
	return oid, ok
}

// ResolveCommit resolves a ref-ish (branch, full ref, or commit oid) to a
// commit oid.
func (v *Vault) ResolveCommit(r *Repo, refish string) (string, bool) {
	if tip, ok := v.BranchTip(r, refish); ok {
		return v.PeelToCommit(tip)
	}
	obj := v.Objects[refish]
	if obj == nil {
		return "", false
	}
	if obj.Type == ObjCommit {
		return refish, true
	}
	if obj.Type == ObjTag {
		return v.PeelToCommit(refish)
	}
	return "", false
}

type WriteOutcome struct {
	OK     bool
	Status int
	Prob   string
	Commit string
	Blob   string // "" when the write deleted the file
}

// WriteFile writes (or, with content nil, deletes) a file on a branch,
// committing on top of the branch tip. It creates the branch when the
// repository is still unborn or when the branch is missing and the
// repository has a default tip to start from.
func (v *Vault) WriteFile(r *Repo, branch, path string, content []byte, message string, expectedSHA string, hasSHA bool) WriteOutcome {
	fullRef := "refs/heads/" + branch
	tip, hasTip := r.Refs[fullRef]
	if !hasTip {
		if fb, ok := r.Refs[v.HeadRef(r)]; ok && fullRef != v.HeadRef(r) {
			tip, hasTip = fb, true
		}
	}
	var base string
	var baseOK bool
	if hasTip {
		base, baseOK = v.PeelToCommit(tip)
	}
	flat := map[string]struct {
		OID  string
		Mode string
	}{}
	if baseOK {
		flat = v.CommitFiles(base)
	}
	prior, priorOK := flat[path]
	if content == nil {
		if !priorOK {
			return WriteOutcome{OK: false, Status: 404, Prob: "Not Found"}
		}
		if hasSHA && expectedSHA != prior.OID {
			return WriteOutcome{OK: false, Status: 409, Prob: "Conflict: the file has changed since the given sha"}
		}
		delete(flat, path)
		treeOID := v.TreeFromFiles(flat)
		var parents []string
		if baseOK {
			parents = []string{base}
		}
		commitOID := v.MakeCommit(treeOID, parents, message, v.Seq())
		r.Refs[fullRef] = commitOID
		return WriteOutcome{OK: true, Commit: commitOID}
	}
	if priorOK && hasSHA && expectedSHA != prior.OID {
		return WriteOutcome{OK: false, Status: 409, Prob: "Conflict: the file has changed since the given sha"}
	}
	if !priorOK && hasSHA {
		return WriteOutcome{OK: false, Status: 422, Prob: "Invalid request: sha supplied for a file that does not exist"}
	}
	blobOID := v.PutObject(ObjBlob, content)
	mode := "100644"
	if priorOK {
		mode = prior.Mode
	}
	flat[path] = struct {
		OID  string
		Mode string
	}{blobOID, mode}
	treeOID := v.TreeFromFiles(flat)
	var parents []string
	if baseOK {
		parents = []string{base}
	}
	commitOID := v.MakeCommit(treeOID, parents, message, v.Seq())
	r.Refs[fullRef] = commitOID
	return WriteOutcome{OK: true, Commit: commitOID, Blob: blobOID}
}

// FileExists reports whether a branch holds a file at a path.
func (v *Vault) FileExists(r *Repo, branch, path string) bool {
	tip, ok := v.BranchTip(r, branch)
	if !ok {
		return false
	}
	commit, ok := v.PeelToCommit(tip)
	if !ok {
		return false
	}
	hit := v.PathAt(commit, path)
	return hit != nil && hit.Kind == "file"
}

// ---------------------------------------------------------------------------
// Issues and pull requests
// ---------------------------------------------------------------------------

func (v *Vault) AddIssue(r *Repo, title, body string, labels []string) *IssueRec {
	num := r.NextNumber
	r.NextNumber++
	v.nextIssueID++
	rec := &IssueRec{
		Number: num,
		ID:     v.nextIssueID,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string{}, labels...),
		Seq:    v.Seq(),
	}
	r.Issues[num] = rec
	return rec
}

func (v *Vault) AddComment(target interface {
	AppendComment(*CommentRec)
}, body string) *CommentRec {
	v.nextCmtID++
	rec := &CommentRec{ID: v.nextCmtID, Body: body, Seq: v.Seq()}
	target.AppendComment(rec)
	return rec
}

func (i *IssueRec) AppendComment(c *CommentRec) { i.Comments = append(i.Comments, c) }
func (p *PullRec) AppendComment(c *CommentRec)  { p.Comments = append(p.Comments, c) }

type CreatePullOutcome struct {
	OK    bool
	Prob  string
	Pull  *PullRec
}

func (v *Vault) CreatePull(r *Repo, title, body, head, base string) CreatePullOutcome {
	headName := strings.TrimPrefix(head, "refs/heads/")
	baseName := strings.TrimPrefix(base, "refs/heads/")
	headOID, ok := v.BranchTip(r, headName)
	if !ok {
		return CreatePullOutcome{Prob: "head branch " + headName + " does not exist"}
	}
	baseOID, ok := v.BranchTip(r, baseName)
	if !ok {
		return CreatePullOutcome{Prob: "base branch " + baseName + " does not exist"}
	}
	for _, ex := range r.Pulls {
		if ex.State == "open" && ex.HeadRef == headName && ex.BaseRef == baseName {
			return CreatePullOutcome{Prob: "a pull request already exists for " + headName + " into " + baseName}
		}
	}
	num := r.NextNumber
	r.NextNumber++
	v.nextIssueID++
	headCommit, _ := v.PeelToCommit(headOID)
	baseCommit, _ := v.PeelToCommit(baseOID)
	rec := &PullRec{
		Number:  num,
		ID:      v.nextIssueID,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: headName,
		BaseRef: baseName,
		HeadSHA: headCommit,
		BaseSHA: baseCommit,
		Seq:     v.Seq(),
	}
	r.Pulls[num] = rec
	return CreatePullOutcome{OK: true, Pull: rec}
}

type PullView struct {
	Base       string
	HeadCommit string
	BaseCommit string
	Conflict   bool
}

// PullView returns the live merge picture of a pull request.
func (v *Vault) PullView(r *Repo, p *PullRec) (PullView, bool) {
	rawHead, ok := v.BranchTip(r, p.HeadRef)
	if !ok {
		return PullView{}, false
	}
	rawBase, ok := v.BranchTip(r, p.BaseRef)
	if !ok {
		return PullView{}, false
	}
	headCommit, ok1 := v.PeelToCommit(rawHead)
	baseCommit, ok2 := v.PeelToCommit(rawBase)
	if !ok1 || !ok2 {
		return PullView{}, false
	}
	common := v.MergeBase(headCommit, baseCommit)
	if common == "" {
		return PullView{Base: "", HeadCommit: headCommit, BaseCommit: baseCommit, Conflict: true}, true
	}
	filesBase := v.CommitFiles(common)
	filesHead := v.CommitFiles(headCommit)
	filesTip := v.CommitFiles(baseCommit)
	conflict := false
	all := map[string]bool{}
	for k := range filesBase {
		all[k] = true
	}
	for k := range filesHead {
		all[k] = true
	}
	for k := range filesTip {
		all[k] = true
	}
	for path := range all {
		b, bOK := filesBase[path]
		h, hOK := filesHead[path]
		t, tOK := filesTip[path]
		headMoved := !hOK || !bOK || h.OID != b.OID
		if hOK && bOK && h.OID == b.OID {
			headMoved = false
		}
		if !hOK && !bOK {
			headMoved = false
		}
		baseMoved := !tOK || !bOK || t.OID != b.OID
		if tOK && bOK && t.OID == b.OID {
			baseMoved = false
		}
		if !tOK && !bOK {
			baseMoved = false
		}
		_ = b
		if headMoved && baseMoved {
			hOID := ""
			if hOK {
				hOID = h.OID
			}
			tOID := ""
			if tOK {
				tOID = t.OID
			}
			if hOID != tOID {
				conflict = true
			}
		}
	}
	return PullView{Base: common, HeadCommit: headCommit, BaseCommit: baseCommit, Conflict: conflict}, true
}

type FileChange struct {
	Path      string
	Status    string
	Additions int
	Deletions int
}

// PullFiles lists the changed files of a pull request (merge-base -> head).
func (v *Vault) PullFiles(r *Repo, p *PullRec) []FileChange {
	headRaw, _ := v.BranchTip(r, p.HeadRef)
	if headRaw == "" {
		headRaw = p.HeadSHA
	}
	baseRaw, _ := v.BranchTip(r, p.BaseRef)
	if baseRaw == "" {
		baseRaw = p.BaseSHA
	}
	headOID, _ := v.PeelToCommit(headRaw)
	baseOID, _ := v.PeelToCommit(baseRaw)
	common := v.MergeBase(headOID, baseOID)
	if common == "" {
		common = baseOID
	}
	return DiffFiles(v, v.CommitFiles(common), v.CommitFiles(headOID))
}

type MergeOutcome struct {
	OK     bool
	Status int
	Prob   string
	SHA    string
}

// MergePull merges a pull request into its base branch: 'squash' lands one
// commit with the head's changes, 'merge' a two-parent commit, 'rebase'
// replays the head's own commits. A conflicted pull is refused with 405.
func (v *Vault) MergePull(r *Repo, p *PullRec, method string) MergeOutcome {
	if p.Merged {
		return MergeOutcome{Status: 405, Prob: "Pull Request was already merged"}
	}
	if p.State == "closed" {
		return MergeOutcome{Status: 405, Prob: "Pull Request is closed"}
	}
	view, ok := v.PullView(r, p)
	if !ok {
		return MergeOutcome{Status: 405, Prob: "Pull Request is not mergeable"}
	}
	if view.Conflict {
		return MergeOutcome{Status: 405, Prob: "Pull Request is not mergeable: conflicts must be resolved first"}
	}
	filesBase := v.CommitFiles(view.Base)
	filesHead := v.CommitFiles(view.HeadCommit)
	combined := v.CommitFiles(view.BaseCommit)
	// Apply the head's changes relative to the merge base onto the base tip.
	for path, he := range filesHead {
		be, inBase := filesBase[path]
		if !inBase || he.OID != be.OID {
			combined[path] = he
		}
	}
	for path := range filesBase {
		if _, inHead := filesHead[path]; !inHead {
			if ce, ok := combined[path]; ok && ce.OID == filesBase[path].OID {
				delete(combined, path)
			}
		}
	}
	mergedTree := v.TreeFromFiles(combined)
	var landed string
	switch method {
	case "merge":
		landed = v.MakeCommit(mergedTree, []string{view.BaseCommit, view.HeadCommit}, "Merge pull request #"+strconv.Itoa(p.Number)+": "+p.Title, v.Seq())
	case "rebase":
		var chain []string
		seen := map[string]bool{}
		var gather func(cur string)
		gather = func(cur string) {
			if cur == view.Base || seen[cur] {
				return
			}
			obj := v.GetObject(cur)
			if obj == nil || obj.Type != ObjCommit {
				return
			}
			seen[cur] = true
			chain = append(chain, cur)
			for _, par := range ParseCommit(obj.Data).Parents {
				gather(par)
			}
		}
		gather(view.HeadCommit)
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		cursor := view.BaseCommit
		landed = view.BaseCommit
		for _, oldOID := range chain {
			oldInfo := ParseCommit(v.GetObject(oldOID).Data)
			landed = v.MakeCommit(oldInfo.Tree, []string{cursor}, oldInfo.Message, v.Seq())
			cursor = landed
		}
		if len(chain) == 0 {
			landed = v.MakeCommit(mergedTree, []string{view.BaseCommit}, p.Title, v.Seq())
		}
	default: // squash
		landed = v.MakeCommit(mergedTree, []string{view.BaseCommit}, p.Title, v.Seq())
	}
	r.Refs["refs/heads/"+p.BaseRef] = landed
	p.State = "closed"
	p.Merged = true
	p.MergeCommitSHA = landed
	p.MergedSeq = v.Seq()
	return MergeOutcome{OK: true, SHA: landed}
}

// CountLineDelta counts line additions/deletions between two file versions
// (multi-set line difference).
func CountLineDelta(oldData, newData []byte) (int, int) {
	tally := func(data []byte) map[string]int {
		counts := map[string]int{}
		if data == nil {
			return counts
		}
		for _, line := range strings.Split(string(data), "\n") {
			counts[line]++
		}
		return counts
	}
	before := tally(oldData)
	after := tally(newData)
	added, removed := 0, 0
	for line, n := range after {
		if d := n - before[line]; d > 0 {
			added += d
		}
	}
	for line, n := range before {
		if d := n - after[line]; d > 0 {
			removed += d
		}
	}
	return added, removed
}

// DiffFiles diffs two flattened file maps into the pull-request file list.
func DiffFiles(v *Vault, before, after map[string]struct {
	OID  string
	Mode string
}) []FileChange {
	all := map[string]bool{}
	for k := range before {
		all[k] = true
	}
	for k := range after {
		all[k] = true
	}
	paths := make([]string, 0, len(all))
	for k := range all {
		paths = append(paths, k)
	}
	sortStrings(paths)
	var rows []FileChange
	for _, path := range paths {
		b, bOK := before[path]
		a, aOK := after[path]
		if bOK && aOK && b.OID == a.OID {
			continue
		}
		var oldData, newData []byte
		if bOK {
			if o := v.GetObject(b.OID); o != nil {
				oldData = o.Data
			}
		}
		if aOK {
			if o := v.GetObject(a.OID); o != nil {
				newData = o.Data
			}
		}
		add, del := CountLineDelta(oldData, newData)
		status := "modified"
		if !bOK {
			status = "added"
		} else if !aOK {
			status = "removed"
		}
		rows = append(rows, FileChange{Path: path, Status: status, Additions: add, Deletions: del})
	}
	return rows
}
