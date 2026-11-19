// GraphQL v4 at POST /api/graphql: the documents gh and github-mcp-server
// actually send — viewer, repository reads, issue/pull-request connections
// with cursor pagination, the create/close/merge mutations, Relay
// node(id:), batching, and the __type introspection probes. A small
// hand-written lexer/parser plus a resolver executor over the shared Vault
// store; no schema library, no dependencies.
//
// Global ids are opaque base64 ("R:<repoId>", "I:<repoId>:<number>", ...) —
// stable across reads and never the plain database id, which the suite's
// global-id check requires.

package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// ids
// ---------------------------------------------------------------------------

func encodeGlobalID(parts ...any) string {
	var segs []string
	for _, p := range parts {
		switch t := p.(type) {
		case string:
			segs = append(segs, t)
		case int:
			segs = append(segs, strconv.Itoa(t))
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(segs, ":")))
}

func decodeGlobalID(id string) []string {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil
	}
	return parts
}

// ---------------------------------------------------------------------------
// lexer and parser
// ---------------------------------------------------------------------------

type tokKind int

const (
	tName tokKind = iota
	tInt
	tFloat
	tString
	tPunct
	tSpread
	tEOF
)

type token struct {
	kind tokKind
	str  string
	num  float64
}

func lexGQL(input string) []token {
	var toks []token
	pos := 0
	for pos < len(input) {
		c := input[pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == 0xEF:
			pos++
		case c == '#':
			for pos < len(input) && input[pos] != '\n' {
				pos++
			}
		case c == '.' && pos+2 < len(input) && input[pos+1] == '.' && input[pos+2] == '.':
			toks = append(toks, token{kind: tSpread})
			pos += 3
		case strings.IndexByte("!$():=@[]{}|", c) >= 0:
			toks = append(toks, token{kind: tPunct, str: string(c)})
			pos++
		case c == '"':
			if pos+2 < len(input) && input[pos+1] == '"' && input[pos+2] == '"' {
				end := strings.Index(input[pos+3:], `"""`)
				if end < 0 {
					toks = append(toks, token{kind: tString, str: input[pos+3:]})
					pos = len(input)
				} else {
					toks = append(toks, token{kind: tString, str: input[pos+3 : pos+3+end]})
					pos = pos + 3 + end + 3
				}
			} else {
				scan := pos + 1
				var sb strings.Builder
				for scan < len(input) && input[scan] != '"' {
					if input[scan] == '\\' && scan+1 < len(input) {
						e := input[scan+1]
						switch e {
						case 'n':
							sb.WriteByte('\n')
						case 't':
							sb.WriteByte('\t')
						case 'r':
							sb.WriteByte('\r')
						default:
							sb.WriteByte(e)
						}
						scan += 2
					} else {
						sb.WriteByte(input[scan])
						scan++
					}
				}
				toks = append(toks, token{kind: tString, str: sb.String()})
				pos = scan + 1
			}
		case (c >= '0' && c <= '9') || c == '-':
			scan := pos
			var sb strings.Builder
			for scan < len(input) {
				ch := input[scan]
				if (ch >= '0' && ch <= '9') || ch == '.' || ch == 'e' || ch == 'E' || ch == '+' || ch == '-' {
					sb.WriteByte(ch)
					scan++
				} else {
					break
				}
			}
			text := sb.String()
			num, _ := strconv.ParseFloat(text, 64)
			if strings.ContainsAny(text, ".eE") {
				toks = append(toks, token{kind: tFloat, num: num})
			} else {
				toks = append(toks, token{kind: tInt, num: num})
			}
			pos = scan
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			scan := pos
			var sb strings.Builder
			for scan < len(input) {
				ch := input[scan]
				if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
					sb.WriteByte(ch)
					scan++
				} else {
					break
				}
			}
			toks = append(toks, token{kind: tName, str: sb.String()})
			pos = scan
		default:
			// Unknown character: skip it; the parser will fail on the
			// resulting shape if it mattered.
			pos++
		}
	}
	toks = append(toks, token{kind: tEOF})
	return toks
}

type gqlValue struct {
	kind  string // var, int, float, string, bool, null, enum, list, object
	num   float64
	str   string
	b     bool
	items []*gqlValue
	fields map[string]*gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sel   []gqlSel
	hasSel bool
}

type gqlSel struct {
	kind string // field, inline, spread
	field *gqlField
	onType string
	sel   []gqlSel
	name  string
}

type gqlOp struct {
	typ  string // query | mutation
	name string
	sel  []gqlSel
}

type gqlDoc struct {
	ops  []gqlOp
	frags map[string]struct {
		onType string
		sel    []gqlSel
	}
}

type gqlParser struct {
	toks []token
	idx  int
}

func (p *gqlParser) peek() token  { return p.toks[p.idx] }
func (p *gqlParser) next() token  { t := p.toks[p.idx]; p.idx++; return t }
func (p *gqlParser) expectPunct(v string) bool {
	t := p.next()
	return t.kind == tPunct && t.str == v
}
func (p *gqlParser) eatPunct(v string) bool {
	t := p.peek()
	if t.kind == tPunct && t.str == v {
		p.idx++
		return true
	}
	return false
}
func (p *gqlParser) expectName() (string, bool) {
	t := p.next()
	if t.kind != tName {
		return "", false
	}
	return t.str, true
}

func (p *gqlParser) parseDocument() (*gqlDoc, bool) {
	doc := &gqlDoc{frags: map[string]struct {
		onType string
		sel    []gqlSel
	}{}}
	for p.peek().kind != tEOF {
		head := p.peek()
		if head.kind == tPunct && head.str == "{" {
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			doc.ops = append(doc.ops, gqlOp{typ: "query", sel: sel})
			continue
		}
		word, ok := p.expectName()
		if !ok {
			return nil, false
		}
		if word == "query" || word == "mutation" {
			var opName string
			if p.peek().kind == tName {
				opName, _ = p.expectName()
			}
			if p.peek().kind == tPunct && p.peek().str == "(" {
				if !p.parseVarDefs() {
					return nil, false
				}
			}
			p.skipDirectives()
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			doc.ops = append(doc.ops, gqlOp{typ: word, name: opName, sel: sel})
		} else if word == "fragment" {
			fragName, ok := p.expectName()
			if !ok {
				return nil, false
			}
			onKw, ok := p.expectName()
			if !ok || onKw != "on" {
				return nil, false
			}
			onType, ok := p.expectName()
			if !ok {
				return nil, false
			}
			p.skipDirectives()
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			doc.frags[fragName] = struct {
				onType string
				sel    []gqlSel
			}{onType, sel}
		} else {
			return nil, false
		}
	}
	return doc, true
}

func (p *gqlParser) parseVarDefs() bool {
	if !p.expectPunct("(") {
		return false
	}
	for !p.eatPunct(")") {
		if !p.expectPunct("$") {
			return false
		}
		if _, ok := p.expectName(); !ok {
			return false
		}
		if !p.expectPunct(":") {
			return false
		}
		if !p.parseType() {
			return false
		}
		if p.eatPunct("=") {
			if _, ok := p.parseValue(); !ok {
				return false
			}
		}
		p.skipDirectives()
		if p.peek().kind == tEOF {
			return false
		}
	}
	return true
}

func (p *gqlParser) parseType() bool {
	if p.eatPunct("[") {
		if !p.parseType() {
			return false
		}
		if !p.expectPunct("]") {
			return false
		}
	} else {
		if _, ok := p.expectName(); !ok {
			return false
		}
	}
	p.eatPunct("!")
	return true
}

func (p *gqlParser) skipDirectives() {
	for p.peek().kind == tPunct && p.peek().str == "@" {
		p.next()
		if _, ok := p.expectName(); !ok {
			return
		}
		if p.peek().kind == tPunct && p.peek().str == "(" {
			p.parseArgs()
		}
	}
}

func (p *gqlParser) parseArgs() (map[string]*gqlValue, bool) {
	args := map[string]*gqlValue{}
	if !p.expectPunct("(") {
		return nil, false
	}
	for !p.eatPunct(")") {
		name, ok := p.expectName()
		if !ok {
			return nil, false
		}
		if !p.expectPunct(":") {
			return nil, false
		}
		val, ok := p.parseValue()
		if !ok {
			return nil, false
		}
		args[name] = val
	}
	return args, true
}

func (p *gqlParser) parseValue() (*gqlValue, bool) {
	t := p.next()
	switch t.kind {
	case tPunct:
		switch t.str {
		case "$":
			name, ok := p.expectName()
			if !ok {
				return nil, false
			}
			return &gqlValue{kind: "var", str: name}, true
		case "[":
			v := &gqlValue{kind: "list"}
			for !p.eatPunct("]") {
				item, ok := p.parseValue()
				if !ok {
					return nil, false
				}
				v.items = append(v.items, item)
			}
			return v, true
		case "{":
			v := &gqlValue{kind: "object", fields: map[string]*gqlValue{}}
			for !p.eatPunct("}") {
				name, ok := p.expectName()
				if !ok {
					return nil, false
				}
				if !p.expectPunct(":") {
					return nil, false
				}
				val, ok := p.parseValue()
				if !ok {
					return nil, false
				}
				v.fields[name] = val
			}
			return v, true
		}
	case tInt:
		return &gqlValue{kind: "int", num: t.num}, true
	case tFloat:
		return &gqlValue{kind: "float", num: t.num}, true
	case tString:
		return &gqlValue{kind: "string", str: t.str}, true
	case tName:
		switch t.str {
		case "true":
			return &gqlValue{kind: "bool", b: true}, true
		case "false":
			return &gqlValue{kind: "bool", b: false}, true
		case "null":
			return &gqlValue{kind: "null"}, true
		}
		return &gqlValue{kind: "enum", str: t.str}, true
	}
	return nil, false
}

func (p *gqlParser) parseSelectionSet() ([]gqlSel, bool) {
	if !p.expectPunct("{") {
		return nil, false
	}
	var entries []gqlSel
	for !p.eatPunct("}") {
		if p.peek().kind == tSpread {
			p.next()
			if p.peek().kind == tName && p.peek().str == "on" {
				p.next()
				onType, ok := p.expectName()
				if !ok {
					return nil, false
				}
				p.skipDirectives()
				sel, ok := p.parseSelectionSet()
				if !ok {
					return nil, false
				}
				entries = append(entries, gqlSel{kind: "inline", onType: onType, sel: sel})
			} else {
				name, ok := p.expectName()
				if !ok {
					return nil, false
				}
				entries = append(entries, gqlSel{kind: "spread", name: name})
			}
			continue
		}
		fieldName, ok := p.expectName()
		if !ok {
			return nil, false
		}
		var alias string
		if p.eatPunct(":") {
			alias = fieldName
			fieldName, ok = p.expectName()
			if !ok {
				return nil, false
			}
		}
		var args map[string]*gqlValue
		if p.peek().kind == tPunct && p.peek().str == "(" {
			args, ok = p.parseArgs()
			if !ok {
				return nil, false
			}
		} else {
			args = map[string]*gqlValue{}
		}
		p.skipDirectives()
		f := &gqlField{alias: alias, name: fieldName, args: args}
		if p.peek().kind == tPunct && p.peek().str == "{" {
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			f.sel = sel
			f.hasSel = true
		}
		entries = append(entries, gqlSel{kind: "field", field: f})
	}
	return entries, true
}

// valueOf evaluates an argument value against the operation's variables.
func valueOf(v *gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		return vars[v.str]
	case "int", "float":
		return v.num
	case "string":
		return v.str
	case "bool":
		return v.b
	case "null":
		return nil
	case "enum":
		return v.str
	case "list":
		out := make([]any, 0, len(v.items))
		for _, item := range v.items {
			out = append(out, valueOf(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, val := range v.fields {
			out[k] = valueOf(val, vars)
		}
		return out
	}
	return nil
}

func argsOf(f *gqlField, vars map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range f.args {
		out[k] = valueOf(v, vars)
	}
	return out
}

// ---------------------------------------------------------------------------
// schema: type field lists for the introspection probes
// ---------------------------------------------------------------------------

var typeFields = map[string][]string{
	"Query":        {"viewer", "repository", "node", "nodes", "rateLimit", "search"},
	"Mutation":     {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment", "reopenIssue"},
	"User":         {"login", "id", "name", "email", "url"},
	"Organization": {"login", "id", "name"},
	"Ref":          {"name", "target", "repository"},
	"Commit":       {"oid", "message", "author"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "description", "isPrivate", "viewerPermission",
		"hasIssuesEnabled", "defaultBranchRef", "issue", "issues", "pullRequest", "pullRequests",
		"createdAt", "updatedAt", "url", "labels", "refs", "ref", "stargazerCount", "forkCount",
	},
	"Issue": {
		"id", "number", "title", "state", "body", "author", "comments", "labels", "repository",
		"createdAt", "updatedAt", "url", "closed", "closedAt",
	},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName",
		"files", "author", "commits", "comments", "mergeCommit", "additions", "deletions", "changedFiles",
		"createdAt", "updatedAt", "url", "closed", "mergedAt",
	},
	"IssueComment": {"id", "body", "author", "createdAt", "updatedAt", "url"},
	"PageInfo":     {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"IssueConnection":       {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":       {"node", "cursor"},
	"PullRequestEdge": {"node", "cursor"},
	"Label":           {"id", "name", "color", "description"},
	"LabelConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"CommentConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"CreateIssuePayload":      {"issue", "clientMutationId"},
	"CloseIssuePayload":       {"issue", "clientMutationId"},
	"CreatePullRequestPayload": {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"AddCommentPayload":        {"commentEdge", "subject", "clientMutationId"},
	"CommentEdge":              {"node", "cursor"},
	"__Type":  {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "ofType"},
	"__Field": {"name", "description", "type", "args", "isDeprecated", "deprecationReason"},
	"__EnumValue":  {"name", "description", "isDeprecated", "deprecationReason"},
	"__InputValue": {"name", "description", "type", "defaultValue"},
}

var enumValues = map[string][]string{
	"IssueState":                   {"OPEN", "CLOSED"},
	"PullRequestState":             {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":               {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod":       {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":         {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"SearchType":                   {"ISSUE", "REPOSITORY", "USER"},
	"IssueOrderField":              {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":               {"ASC", "DESC"},
	"PullRequestChangedFileChangeType": {"ADDED", "MODIFIED", "DELETED", "RENAMED"},
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type gqlCtx struct {
	app       *App
	variables map[string]any
	frags     map[string]struct {
		onType string
		sel    []gqlSel
	}
	errors []map[string]any
}

// A node is a map carrying __typename plus typed payload fields.
type gqlNode map[string]any

func userNode(a *App) gqlNode {
	return gqlNode{"__typename": "User", "login": a.vault.Login}
}
func repoNode(a *App, r *Repo) gqlNode {
	return gqlNode{"__typename": "Repository", "repo": r}
}
func issueNode(r *Repo, i *IssueRec) gqlNode {
	return gqlNode{"__typename": "Issue", "repo": r, "issue": i}
}
func pullNode(r *Repo, p *PullRec) gqlNode {
	return gqlNode{"__typename": "PullRequest", "repo": r, "pull": p}
}

func cursorOf(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func numberInCursor(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (c *gqlCtx) executeSelection(typeName string, src gqlNode, sel []gqlSel) map[string]any {
	out := map[string]any{}
	for _, entry := range sel {
		switch entry.kind {
		case "inline":
			if src != nil && src["__typename"] == entry.onType {
				for k, v := range c.executeSelection(entry.onType, src, entry.sel) {
					out[k] = v
				}
			}
		case "spread":
			if frag, ok := c.frags[entry.name]; ok && src != nil && src["__typename"] == frag.onType {
				for k, v := range c.executeSelection(frag.onType, src, frag.sel) {
					out[k] = v
				}
			}
		case "field":
			f := entry.field
			key := f.name
			if f.alias != "" {
				key = f.alias
			}
			if f.name == "__typename" {
				out[key] = typeName
				continue
			}
			resolved := c.resolveField(typeName, src, f)
			out[key] = c.completeValue(resolved, f)
		}
	}
	return out
}

// completeValue applies a field's selection set to its resolved value.
func (c *gqlCtx) completeValue(resolved any, f *gqlField) any {
	if resolved == nil {
		return nil
	}
	if !f.hasSel {
		return resolved
	}
	if arr, ok := resolved.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, e := range arr {
			out = append(out, c.completeValue(e, f))
		}
		return out
	}
	if node, ok := resolved.(gqlNode); ok {
		return c.executeSelection(node["__typename"].(string), node, f.sel)
	}
	if m, ok := resolved.(map[string]any); ok {
		if tn, ok := m["__typename"].(string); ok {
			return c.executeSelection(tn, m, f.sel)
		}
	}
	return resolved
}

func (c *gqlCtx) resolveField(typeName string, src gqlNode, f *gqlField) any {
	v := c.app.vault
	args := argsOf(f, c.variables)
	if known, ok := typeFields[typeName]; ok {
		found := false
		for _, k := range known {
			if k == f.name {
				found = true
				break
			}
		}
		if !found && f.name != "__type" && f.name != "__schema" {
			c.errors = append(c.errors, map[string]any{"message": "Cannot query field \"" + f.name + "\" on type \"" + typeName + "\"."})
			return nil
		}
	}
	switch typeName {
	case "Query":
		return c.resolveQuery(f.name, args)
	case "Mutation":
		return c.resolveMutation(f.name, args)
	case "User":
		switch f.name {
		case "login":
			if s, ok := src["login"].(string); ok {
				return s
			}
			return v.Login
		case "id":
			return encodeGlobalID("U", 1)
		case "name":
			return v.Login
		}
		return nil
	case "Repository":
		return c.resolveRepository(src, f.name, args)
	case "Issue":
		issue := src["issue"].(*IssueRec)
		repo := src["repo"].(*Repo)
		switch f.name {
		case "id":
			return encodeGlobalID("I", repo.ID, issue.Number)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			return strings.ToUpper(issue.State)
		case "author":
			return userNode(c.app)
		case "repository":
			return repoNode(c.app, repo)
		case "createdAt", "updatedAt":
			return v.ISOAtSeq(issue.Seq)
		case "closed":
			return issue.State == "closed"
		case "labels":
			nodes := []any{}
			for _, name := range issue.Labels {
				color := "ededed"
				if rec, ok := repo.Labels[name]; ok {
					color = rec.Color
				}
				nodes = append(nodes, gqlNode{"__typename": "Label", "name": name, "color": color})
			}
			return gqlNode{"__typename": "LabelConnection", "nodes": nodes}
		case "comments":
			nodes := []any{}
			for _, e := range issue.Comments {
				nodes = append(nodes, c.commentNode(repo, issue.Number, e))
			}
			return gqlNode{"__typename": "CommentConnection", "nodes": nodes}
		}
		return nil
	case "PullRequest":
		pull := src["pull"].(*PullRec)
		repo := src["repo"].(*Repo)
		switch f.name {
		case "id":
			return encodeGlobalID("P", repo.ID, pull.Number)
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pull.State)
		case "merged":
			return pull.Merged
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "mergeable":
			return c.pullMergeable(repo, pull)
		case "author":
			return userNode(c.app)
		case "createdAt", "updatedAt":
			return v.ISOAtSeq(pull.Seq)
		case "closed":
			return pull.State == "closed"
		case "mergedAt":
			if pull.Merged {
				return v.ISOAtSeq(pull.MergedSeq)
			}
			return nil
		case "mergeCommit":
			if pull.MergeCommitSHA == "" {
				return nil
			}
			return gqlNode{"__typename": "Commit", "oid": pull.MergeCommitSHA}
		case "files":
			changed := v.PullFiles(repo, pull)
			limit := len(changed)
			if n, ok := args["first"].(float64); ok {
				limit = int(n)
			}
			if limit > len(changed) {
				limit = len(changed)
			}
			nodes := []any{}
			for _, row := range changed[:limit] {
				nodes = append(nodes, gqlNode{"__typename": "PullRequestChangedFile", "path": row.Path, "additions": row.Additions, "deletions": row.Deletions})
			}
			return gqlNode{"__typename": "PullRequestChangedFileConnection", "nodes": nodes, "totalCount": len(changed)}
		case "additions", "deletions", "changedFiles":
			changed := v.PullFiles(repo, pull)
			if f.name == "changedFiles" {
				return len(changed)
			}
			total := 0
			for _, row := range changed {
				if f.name == "additions" {
					total += row.Additions
				} else {
					total += row.Deletions
				}
			}
			return total
		case "comments":
			nodes := []any{}
			for _, e := range pull.Comments {
				nodes = append(nodes, c.commentNode(repo, pull.Number, e))
			}
			return gqlNode{"__typename": "CommentConnection", "nodes": nodes}
		}
		return nil
	case "Ref":
		switch f.name {
		case "name":
			return src["name"]
		case "target":
			return gqlNode{"__typename": "Commit", "oid": src["oid"]}
		}
		return nil
	case "Commit":
		if f.name == "oid" {
			return src["oid"]
		}
		return nil
	case "PageInfo":
		return src[f.name]
	case "__Type":
		def := src["typeDef"].(map[string]any)
		name := def["name"].(string)
		switch f.name {
		case "name":
			return name
		case "kind":
			return def["kind"]
		case "fields":
			var out []any
			for _, fn := range typeFields[name] {
				out = append(out, gqlNode{"__typename": "__Field", "name": fn})
			}
			return out
		case "enumValues":
			var out []any
			for _, en := range enumValues[name] {
				out = append(out, gqlNode{"__typename": "__EnumValue", "name": en})
			}
			return out
		}
		return nil
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return src["name"]
		}
		return nil
	}
	// Connection-shaped sources: nodes / edges / pageInfo / totalCount read
	// straight off the source.
	if src != nil {
		if val, ok := src[f.name]; ok {
			return val
		}
	}
	if _, ok := typeFields[typeName]; !ok {
		return nil
	}
	c.errors = append(c.errors, map[string]any{"message": "Cannot query field \"" + f.name + "\" on type \"" + typeName + "\"."})
	return nil
}

func (c *gqlCtx) commentNode(repo *Repo, subjectNumber int, e *CommentRec) gqlNode {
	v := c.app.vault
	return gqlNode{
		"__typename": "IssueComment",
		"id":         encodeGlobalID("C", repo.ID, subjectNumber, e.ID),
		"body":       e.Body,
		"createdAt":  v.ISOAtSeq(e.Seq),
		"updatedAt":  v.ISOAtSeq(e.Seq),
	}
}

func (c *gqlCtx) pullMergeable(repo *Repo, p *PullRec) string {
	if p.Merged || p.State == "closed" {
		return "UNKNOWN"
	}
	view, ok := c.app.vault.PullView(repo, p)
	if !ok {
		return "UNKNOWN"
	}
	if view.Conflict {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func (c *gqlCtx) resolveQuery(name string, args map[string]any) any {
	v := c.app.vault
	switch name {
	case "viewer":
		return userNode(c.app)
	case "repository":
		owner, _ := args["owner"].(string)
		rname, _ := args["name"].(string)
		found, ok := v.Repos[rname]
		if !ok || !v.OwnerMatches(owner) {
			return nil
		}
		v.AdoptLogin(owner)
		return repoNode(c.app, found)
	case "node":
		id, _ := args["id"].(string)
		return c.nodeByID(id)
	case "__type":
		wanted, _ := args["name"].(string)
		if _, ok := typeFields[wanted]; ok {
			return gqlNode{"__typename": "__Type", "typeDef": map[string]any{"name": wanted, "kind": "OBJECT"}}
		}
		if _, ok := enumValues[wanted]; ok {
			return gqlNode{"__typename": "__Type", "typeDef": map[string]any{"name": wanted, "kind": "ENUM"}}
		}
		return nil
	}
	return nil
}

func (c *gqlCtx) nodeByID(id string) gqlNode {
	v := c.app.vault
	parts := decodeGlobalID(id)
	if parts == nil {
		return nil
	}
	kind, first, second := parts[0], parts[1], ""
	if len(parts) > 2 {
		second = parts[2]
	}
	switch kind {
	case "R":
		n, _ := strconv.Atoi(first)
		for _, r := range v.Repos {
			if r.ID == n {
				return repoNode(c.app, r)
			}
		}
	case "I", "P":
		n, _ := strconv.Atoi(first)
		for _, r := range v.Repos {
			if r.ID != n {
				continue
			}
			num, _ := strconv.Atoi(second)
			if kind == "I" {
				if issue, ok := r.Issues[num]; ok {
					return issueNode(r, issue)
				}
			} else if pull, ok := r.Pulls[num]; ok {
				return pullNode(r, pull)
			}
		}
	case "U":
		return userNode(c.app)
	}
	return nil
}

func (c *gqlCtx) resolveRepository(src gqlNode, name string, args map[string]any) any {
	v := c.app.vault
	repo := src["repo"].(*Repo)
	switch name {
	case "id":
		return encodeGlobalID("R", repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return v.Login + "/" + repo.Name
	case "owner":
		return userNode(c.app)
	case "description":
		return repo.Description
	case "isPrivate":
		return repo.Private
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "createdAt", "updatedAt":
		return v.ISOAtSeq(repo.Seq)
	case "defaultBranchRef":
		tip, ok := v.BranchTip(repo, repo.DefaultBranch)
		if !ok {
			return nil
		}
		return gqlNode{"__typename": "Ref", "name": repo.DefaultBranch, "oid": tip}
	case "issue":
		num, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		if issue, ok := repo.Issues[int(num)]; ok {
			return issueNode(repo, issue)
		}
		return nil
	case "issues":
		return c.issuesConnection(repo, args)
	case "pullRequest":
		num, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		if pull, ok := repo.Pulls[int(num)]; ok {
			return pullNode(repo, pull)
		}
		return nil
	case "pullRequests":
		var every []*PullRec
		for _, p := range repo.Pulls {
			every = append(every, p)
		}
		for i := 1; i < len(every); i++ {
			for j := i; j > 0 && every[j].Number > every[j-1].Number; j-- {
				every[j], every[j-1] = every[j-1], every[j]
			}
		}
		limit := len(every)
		if n, ok := args["first"].(float64); ok {
			limit = int(n)
		}
		if limit > len(every) {
			limit = len(every)
		}
		nodes := []any{}
		for _, p := range every[:limit] {
			nodes = append(nodes, pullNode(repo, p))
		}
		return gqlNode{
			"__typename": "PullRequestConnection", "nodes": nodes, "totalCount": len(every),
			"pageInfo": gqlNode{"__typename": "PageInfo", "hasNextPage": limit < len(every), "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
		}
	}
	return nil
}

func (c *gqlCtx) issuesConnection(repo *Repo, args map[string]any) gqlNode {
	var all []*IssueRec
	for _, i := range repo.Issues {
		all = append(all, i)
	}
	if states, ok := args["states"].([]any); ok {
		var wanted []string
		for _, s := range states {
			if str, ok := s.(string); ok {
				wanted = append(wanted, strings.ToLower(str))
			}
		}
		var filtered []*IssueRec
		for _, i := range all {
			for _, w := range wanted {
				if i.State == w {
					filtered = append(filtered, i)
					break
				}
			}
		}
		all = filtered
	}
	dir := "DESC"
	var order map[string]any
	if o, ok := args["orderBy"].(map[string]any); ok {
		order = o
	}
	field := "CREATED_AT"
	if order != nil {
		if f, ok := order["field"].(string); ok {
			field = f
		}
		if d, ok := order["direction"].(string); ok && d == "ASC" {
			dir = "ASC"
		} else if order == nil {
			dir = "ASC"
		}
	} else {
		dir = "ASC"
	}
	keyOf := func(i *IssueRec) int {
		if field == "COMMENTS" {
			return len(i.Comments)*100000 + int(i.Seq)
		}
		return int(i.Seq)
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			d := keyOf(all[j]) - keyOf(all[j-1])
			swap := false
			if d != 0 {
				swap = (dir == "DESC" && d > 0) || (dir == "ASC" && d < 0)
			} else {
				swap = (dir == "DESC" && all[j].Number > all[j-1].Number) || (dir == "ASC" && all[j].Number < all[j-1].Number)
			}
			if !swap {
				break
			}
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	startAt := 0
	if after, ok := args["after"].(string); ok {
		if n, ok := numberInCursor(after); ok {
			for idx, i := range all {
				if i.Number == n {
					startAt = idx + 1
					break
				}
			}
		}
	}
	limit := len(all)
	if n, ok := args["first"].(float64); ok {
		limit = int(n)
	}
	if startAt+limit > len(all) {
		limit = len(all) - startAt
	}
	if limit < 0 {
		limit = 0
	}
	page := all[startAt : startAt+limit]
	nodes := []any{}
	for _, i := range page {
		nodes = append(nodes, issueNode(repo, i))
	}
	var startCursor, endCursor any
	if len(page) > 0 {
		startCursor = cursorOf(page[0].Number)
		endCursor = cursorOf(page[len(page)-1].Number)
	} else if after, ok := args["after"].(string); ok {
		endCursor = after
	}
	return gqlNode{
		"__typename": "IssueConnection", "nodes": nodes, "totalCount": len(all),
		"pageInfo": gqlNode{"__typename": "PageInfo", "hasNextPage": startAt+limit < len(all), "hasPreviousPage": startAt > 0, "startCursor": startCursor, "endCursor": endCursor},
	}
}

func (c *gqlCtx) resolveMutation(name string, args map[string]any) any {
	v := c.app.vault
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	repoFromID := func(id any) *Repo {
		s, ok := id.(string)
		if !ok {
			return nil
		}
		parts := decodeGlobalID(s)
		if parts == nil || parts[0] != "R" {
			return nil
		}
		n, _ := strconv.Atoi(parts[1])
		for _, r := range v.Repos {
			if r.ID == n {
				return r
			}
		}
		return nil
	}
	switch name {
	case "createIssue":
		repo := repoFromID(input["repositoryId"])
		if repo == nil {
			c.errors = append(c.errors, map[string]any{"message": "repository not found for createIssue"})
			return gqlNode{"__typename": "CreateIssuePayload", "issue": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		created := v.AddIssue(repo, title, body, nil)
		return gqlNode{"__typename": "CreateIssuePayload", "issue": issueNode(repo, created)}
	case "closeIssue":
		idStr, _ := input["issueId"].(string)
		parts := decodeGlobalID(idStr)
		var repo *Repo
		var issue *IssueRec
		if parts != nil && parts[0] == "I" {
			n, _ := strconv.Atoi(parts[1])
			for _, r := range v.Repos {
				if r.ID == n {
					repo = r
					issue = r.Issues[mustAtoi(parts[2])]
				}
			}
		}
		if repo == nil || issue == nil {
			c.errors = append(c.errors, map[string]any{"message": "issue not found for closeIssue"})
			return gqlNode{"__typename": "CloseIssuePayload", "issue": nil}
		}
		issue.State = "closed"
		return gqlNode{"__typename": "CloseIssuePayload", "issue": issueNode(repo, issue)}
	case "createPullRequest":
		repo := repoFromID(input["repositoryId"])
		if repo == nil {
			c.errors = append(c.errors, map[string]any{"message": "repository not found for createPullRequest"})
			return gqlNode{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		head, _ := input["headRefName"].(string)
		base, ok := input["baseRefName"].(string)
		if !ok {
			base = repo.DefaultBranch
		}
		outcome := v.CreatePull(repo, title, body, head, base)
		if !outcome.OK {
			c.errors = append(c.errors, map[string]any{"message": outcome.Prob})
			return gqlNode{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		return gqlNode{"__typename": "CreatePullRequestPayload", "pullRequest": pullNode(repo, outcome.Pull)}
	case "mergePullRequest":
		idStr, _ := input["pullRequestId"].(string)
		parts := decodeGlobalID(idStr)
		var repo *Repo
		var pull *PullRec
		if parts != nil && parts[0] == "P" {
			n, _ := strconv.Atoi(parts[1])
			for _, r := range v.Repos {
				if r.ID == n {
					repo = r
					pull = r.Pulls[mustAtoi(parts[2])]
				}
			}
		}
		if repo == nil || pull == nil {
			c.errors = append(c.errors, map[string]any{"message": "pull request not found for mergePullRequest"})
			return gqlNode{"__typename": "MergePullRequestPayload", "pullRequest": nil}
		}
		method := "squash"
		if m, ok := input["mergeMethod"].(string); ok {
			switch m {
			case "MERGE":
				method = "merge"
			case "REBASE":
				method = "rebase"
			}
		}
		outcome := v.MergePull(repo, pull, method)
		if !outcome.OK {
			c.errors = append(c.errors, map[string]any{"message": outcome.Prob})
			return gqlNode{"__typename": "MergePullRequestPayload", "pullRequest": pullNode(repo, pull)}
		}
		return gqlNode{"__typename": "MergePullRequestPayload", "pullRequest": pullNode(repo, pull)}
	case "addComment":
		idStr, _ := input["subjectId"].(string)
		parts := decodeGlobalID(idStr)
		body, _ := input["body"].(string)
		if parts != nil && (parts[0] == "I" || parts[0] == "P") {
			n, _ := strconv.Atoi(parts[1])
			for _, r := range v.Repos {
				if r.ID != n {
					continue
				}
				num := mustAtoi(parts[2])
				if parts[0] == "I" {
					if issue, ok := r.Issues[num]; ok {
						added := v.AddComment(issue, body)
						node := gqlNode{
							"__typename": "IssueComment",
							"id":         encodeGlobalID("C", r.ID, num, added.ID),
							"body":       added.Body,
							"createdAt":  v.ISOAtSeq(added.Seq),
						}
						return gqlNode{"__typename": "AddCommentPayload", "commentEdge": gqlNode{"__typename": "CommentEdge", "node": node, "cursor": cursorOf(num)}}
					}
				} else if pull, ok := r.Pulls[num]; ok {
					added := v.AddComment(pull, body)
					node := gqlNode{
						"__typename": "IssueComment",
						"id":         encodeGlobalID("C", r.ID, num, added.ID),
						"body":       added.Body,
						"createdAt":  v.ISOAtSeq(added.Seq),
					}
					return gqlNode{"__typename": "AddCommentPayload", "commentEdge": gqlNode{"__typename": "CommentEdge", "node": node, "cursor": cursorOf(num)}}
				}
			}
		}
		c.errors = append(c.errors, map[string]any{"message": "subject not found for addComment"})
		return gqlNode{"__typename": "AddCommentPayload", "commentEdge": nil}
	}
	return nil
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type gqlResult struct {
	data   map[string]any
	errors []map[string]any
}

func (a *App) executeOne(body map[string]any) gqlResult {
	queryText, _ := body["query"].(string)
	vars, _ := body["variables"].(map[string]any)
	if vars == nil {
		vars = map[string]any{}
	}
	p := &gqlParser{toks: lexGQL(queryText)}
	doc, ok := p.parseDocument()
	if !ok {
		return gqlResult{errors: []map[string]any{{"message": "parse error"}}}
	}
	if len(doc.ops) == 0 {
		return gqlResult{errors: []map[string]any{{"message": "no operation in document"}}}
	}
	opName, _ := body["operationName"].(string)
	chosen := doc.ops[0]
	for _, op := range doc.ops {
		if op.name == opName {
			chosen = op
			break
		}
	}
	ctx := &gqlCtx{app: a, variables: vars, frags: doc.frags}
	rootKind := "Query"
	if chosen.typ == "mutation" {
		rootKind = "Mutation"
	}
	data := ctx.executeSelection(rootKind, gqlNode{"__typename": rootKind}, chosen.sel)
	return gqlResult{data: data, errors: ctx.errors}
}

// handleGraphQL serves POST /api/graphql (single operations and JSON-array
// batches).
func (a *App) handleGraphQL(req *Request, res *Response) {
	if !isAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return
	}
	parsed, ok := parseBodyJSON(req)
	if !ok {
		res.SendJSON(400, map[string]any{"errors": []any{map[string]any{"message": "Problems parsing JSON"}}})
		return
	}
	if arr, ok := parsed.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			m, _ := entry.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			results = append(results, a.gqlResultJSON(a.executeOne(m)))
		}
		res.SendJSON(200, results)
		return
	}
	if m, ok := parsed.(map[string]any); ok {
		res.SendJSON(200, a.gqlResultJSON(a.executeOne(m)))
		return
	}
	res.SendJSON(400, map[string]any{"errors": []any{map[string]any{"message": "Expected a GraphQL operation object"}}})
}

func (a *App) gqlResultJSON(r gqlResult) map[string]any {
	out := map[string]any{"data": r.data}
	if len(r.errors) > 0 {
		errs := make([]any, 0, len(r.errors))
		for _, e := range r.errors {
			errs = append(errs, e)
		}
		out["errors"] = errs
	}
	return out
}
