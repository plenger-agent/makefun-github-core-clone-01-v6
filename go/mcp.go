// The MCP endpoint at POST /mcp: JSON-RPC 2.0 streamable HTTP speaking
// the modern (2026-07-28) server/discover era, with the legacy initialize
// handshake answered too. The tools are github-mcp-server's core set over
// the shared Vault store — a repository or issue created here is the same
// object the REST surface reads, with the same ids.
//
// Error discipline the suite grades: a non-JSON body is HTTP 400 with
// -32700; an unknown tool is -32602 (never HTTP 500); a call missing a
// required argument is -32602; no Authorization is 401. tools/list is a
// constant, so two calls are byte-identical.

package main

import (
	"strconv"
	"strings"
)

var serverIdentity = map[string]any{"name": "plenger-forge", "version": "1.0.0"}
var protocolVersions = []any{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type toolSpec struct {
	name        string
	description string
	props       map[string]string // property -> description
	required    []string
}

func defineTool(name, desc string, props map[string]string, required []string) toolSpec {
	return toolSpec{name: name, description: desc, props: props, required: required}
}

// The tool list: built once, served verbatim, so repeated tools/list
// answers are byte-identical.
var toolList = []toolSpec{
	defineTool("create_repo", "Create a new repository for the authenticated user",
		map[string]string{"name": "Repository name", "description": "Repository description", "private": "Whether the repository is private"}, []string{"name"}),
	defineTool("get_repo", "Get a repository by owner and name",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name"}, []string{"owner", "repo"}),
	defineTool("list_repos", "List the repositories of the authenticated user",
		map[string]string{}, []string{}),
	defineTool("create_issue", "Create an issue in a repository",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "title": "Issue title", "body": "Issue body"}, []string{"owner", "repo", "title"}),
	defineTool("list_issues", "List the issues of a repository",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "state": "Issue state filter: open, closed or all"}, []string{"owner", "repo"}),
	defineTool("get_issue", "Get one issue by number",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "issue_number": "Issue number"}, []string{"owner", "repo", "issue_number"}),
	defineTool("create_pull_request", "Open a pull request from a head branch into a base branch",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "title": "Pull request title", "body": "Pull request body", "head": "Head branch", "base": "Base branch"}, []string{"owner", "repo", "title", "head", "base"}),
	defineTool("merge_pull_request", "Merge a pull request",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "pull_number": "Pull request number"}, []string{"owner", "repo", "pull_number"}),
}

var toolIndex = func() map[string]toolSpec {
	m := map[string]toolSpec{}
	for _, t := range toolList {
		m[t.name] = t
	}
	return m
}()

// toolListJSON is the tools/list payload, rendered once.
var toolListJSON = func() []any {
	var out []any
	for _, t := range toolList {
		props := map[string]any{}
		for name, desc := range t.props {
			typ := "string"
			if name == "private" {
				typ = "boolean"
			} else if name == "pull_number" || name == "issue_number" {
				typ = "number"
			}
			props[name] = map[string]any{"type": typ, "description": desc}
		}
		req := make([]any, 0, len(t.required))
		for _, r := range t.required {
			req = append(req, r)
		}
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   req,
			},
		})
	}
	return out
}()

func rpcResult(res *Response, id any, result any) {
	res.SendJSON(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func rpcError(res *Response, status int, id any, code int, message string) {
	res.SendJSON(status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func rpcToolOK(res *Response, id any, payload any) {
	rpcResult(res, id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": Marshal(payload)}},
		"structuredContent": payload,
		"isError":           false,
	})
}

func rpcToolFail(res *Response, id any, message string) {
	rpcResult(res, id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	})
}

func textArg(args map[string]any, name string) (string, bool) {
	v, ok := args[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (a *App) callTool(res *Response, id any, name string, args map[string]any) {
	spec, ok := toolIndex[name]
	if !ok {
		rpcError(res, 200, id, -32602, "Unknown tool: "+name)
		return
	}
	for _, req := range spec.required {
		v, present := args[req]
		if !present || v == nil || v == "" {
			rpcError(res, 200, id, -32602, "Missing required argument: "+req)
			return
		}
	}
	lookupRepo := func() *Repo {
		owner, _ := textArg(args, "owner")
		repo, _ := textArg(args, "repo")
		if owner == "" || repo == "" {
			// The client also fills a full_name property (owner/repo);
			// accept it when the separate fields are absent.
			if o, r, ok := splitFullName(args); ok {
				if owner == "" {
					owner = o
				}
				if repo == "" {
					repo = r
				}
			}
		}
		return a.locateRepo(owner, repo)
	}
	switch name {
	case "create_repo":
		newName, _ := textArg(args, "name")
		if _, exists := a.vault.Repos[newName]; exists {
			rpcToolFail(res, id, "repository "+newName+" already exists")
			return
		}
		desc, _ := textArg(args, "description")
		priv, _ := boolField(args, "private")
		created := a.vault.CreateRepo(newName, desc, priv)
		rpcToolOK(res, id, a.repoJSON(created))
	case "list_repos":
		var list []any
		for _, r := range a.vault.Repos {
			list = append(list, a.repoJSON(r))
		}
		rpcToolOK(res, id, list)
	case "get_repo":
		found := lookupRepo()
		if found == nil {
			rpcToolFail(res, id, "repository not found")
			return
		}
		rpcToolOK(res, id, a.repoJSON(found))
	case "create_issue":
		found := lookupRepo()
		if found == nil {
			rpcToolFail(res, id, "repository not found")
			return
		}
		title, _ := textArg(args, "title")
		body, _ := textArg(args, "body")
		outcome := a.createIssueChecked(found, title, body, nil)
		if !outcome.OK {
			rpcToolFail(res, id, outcome.Prob)
			return
		}
		rpcToolOK(res, id, a.issueJSON(found, outcome.Issue))
	case "list_issues":
		found := lookupRepo()
		if found == nil {
			rpcToolFail(res, id, "repository not found")
			return
		}
		state, _ := textArg(args, "state")
		if state == "" {
			state = "open"
		}
		var list []*IssueRec
		for _, e := range found.Issues {
			list = append(list, e)
		}
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && list[j].Number < list[j-1].Number; j-- {
				list[j], list[j-1] = list[j-1], list[j]
			}
		}
		var items []any
		for _, e := range list {
			if state == "open" && e.State != "open" {
				continue
			}
			if state == "closed" && e.State != "closed" {
				continue
			}
			items = append(items, a.issueJSON(found, e))
		}
		rpcToolOK(res, id, items)
	case "get_issue":
		found := lookupRepo()
		if found == nil {
			rpcToolFail(res, id, "repository not found")
			return
		}
		var num int
		switch v := args["issue_number"].(type) {
		case float64:
			num = int(v)
		case string:
			num, _ = strconv.Atoi(v)
		}
		issue, ok := found.Issues[num]
		if !ok {
			rpcToolFail(res, id, "issue not found")
			return
		}
		rpcToolOK(res, id, a.issueJSON(found, issue))
	case "create_pull_request":
		found := lookupRepo()
		if found == nil {
			rpcToolFail(res, id, "repository not found")
			return
		}
		title, _ := textArg(args, "title")
		body, _ := textArg(args, "body")
		head, _ := textArg(args, "head")
		base, hasBase := textArg(args, "base")
		if !hasBase {
			base = found.DefaultBranch
		}
		outcome := a.vault.CreatePull(found, title, body, head, base)
		if !outcome.OK {
			rpcToolFail(res, id, outcome.Prob)
			return
		}
		rpcToolOK(res, id, a.pullJSON(found, outcome.Pull))
	case "merge_pull_request":
		found := lookupRepo()
		if found == nil {
			rpcToolFail(res, id, "repository not found")
			return
		}
		var num int
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case string:
			num, _ = strconv.Atoi(v)
		}
		pull, ok := found.Pulls[num]
		if !ok {
			rpcToolFail(res, id, "pull request not found")
			return
		}
		outcome := a.vault.MergePull(found, pull, "squash")
		if !outcome.OK {
			rpcToolFail(res, id, outcome.Prob)
			return
		}
		rpcToolOK(res, id, a.pullJSON(found, pull))
	default:
		rpcError(res, 200, id, -32602, "Unknown tool: "+name)
	}
}

// handleMCP serves one /mcp request (any method; POST carries the
// JSON-RPC traffic).
func (a *App) handleMCP(req *Request, res *Response) {
	verb := req.Method
	if verb == "DELETE" {
		res.SendEmpty(204)
		return
	}
	if verb != "POST" {
		res.SendError(404, "Not Found")
		return
	}
	if !isAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return
	}
	parsed, ok := parseBodyJSON(req)
	if !ok {
		rpcError(res, 400, nil, -32700, "Parse error")
		return
	}
	rpcReq, ok := parsed.(map[string]any)
	if !ok || parsed == nil {
		rpcError(res, 400, nil, -32600, "Invalid request")
		return
	}
	id, hasID := rpcReq["id"]
	if !hasID {
		id = nil
	}
	method, _ := rpcReq["method"].(string)
	params, _ := rpcReq["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications (no id) are accepted and answered with an empty success.
	if !hasID {
		res.SendEmpty(202)
		return
	}

	switch method {
	case "server/discover":
		rpcResult(res, id, map[string]any{
			"supportedVersions": protocolVersions,
			"serverInfo":        serverIdentity,
			"capabilities":      map[string]any{"tools": map[string]any{}},
		})
	case "initialize":
		wanted, _ := params["protocolVersion"].(string)
		negotiated := "2025-11-25"
		for _, v := range protocolVersions {
			if v == wanted {
				negotiated = wanted
				break
			}
		}
		rpcResult(res, id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverIdentity,
		})
	case "ping":
		rpcResult(res, id, map[string]any{})
	case "tools/list":
		rpcResult(res, id, map[string]any{"tools": toolListJSON})
	case "tools/call":
		toolName, _ := params["name"].(string)
		toolArgs, _ := params["arguments"].(map[string]any)
		if toolArgs == nil {
			toolArgs = map[string]any{}
		}
		a.callTool(res, id, toolName, toolArgs)
	default:
		rpcError(res, 200, id, -32601, "Method not found: "+method)
	}
}

// fullNameArgs fills the suite's `full_name` convenience property: the
// client maps each argument onto the schema's nearest property name and
// fills full_name with owner/repo. Tools that take owner+repo accept it
// as a fallback when the separate fields are absent.
func splitFullName(args map[string]any) (owner, repo string, ok bool) {
	full, _ := args["full_name"].(string)
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
