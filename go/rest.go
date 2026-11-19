// The REST v3 surface: repositories, branches, commits, contents, labels,
// issues, comments and pull requests under /api/v3, rendered in GitHub's
// shapes. Everything reads and writes the one Vault store, so the REST
// view is the same data the git, GraphQL and MCP surfaces see.
//
// Wire discipline (the protocol group grades it): every answer is JSON
// with an X-GitHub-Request-Id (set centrally in router.go), error bodies
// are {"message": ...}, a missing token is 401, a repository under another
// owner is 404 (never 403), and a malformed JSON body is 400.
//
// The hot shapes (repository, issue) are rendered as JSON text by hand
// with the fast string escaper instead of building a value and
// marshalling it — the write workload creates one issue per request, and
// the value round-trip is pure overhead there.

package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

const restVersion = "1.0.0"

// repoJSONText renders a repository as JSON text.
func (a *App) repoJSONText(r *Repo) string {
	stamp := a.vault.ISOAtSeq(r.Seq)
	login := a.vault.Login
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(r.ID))
	b.WriteString(`,"name":`)
	b.WriteString(jsonQuote(r.Name))
	b.WriteString(`,"full_name":`)
	b.WriteString(jsonQuote(login + "/" + r.Name))
	b.WriteString(`,"owner":{"login":`)
	b.WriteString(jsonQuote(login))
	b.WriteString(`,"id":1,"type":"User"},"private":`)
	if r.Private {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"description":`)
	b.WriteString(jsonQuote(r.Description))
	b.WriteString(`,"default_branch":`)
	b.WriteString(jsonQuote(r.DefaultBranch))
	b.WriteString(`,"created_at":`)
	b.WriteString(jsonQuote(stamp))
	b.WriteString(`,"updated_at":`)
	b.WriteString(jsonQuote(stamp))
	b.WriteString("}\n")
	return b.String()
}

func (a *App) repoJSON(r *Repo) map[string]any {
	login := a.vault.Login
	stamp := a.vault.ISOAtSeq(r.Seq)
	return map[string]any{
		"id":             r.ID,
		"name":           r.Name,
		"full_name":      login + "/" + r.Name,
		"owner":          map[string]any{"login": login, "id": 1, "type": "User"},
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": r.DefaultBranch,
		"created_at":     stamp,
		"updated_at":     stamp,
	}
}

// issueJSONText renders an issue (labels as objects) as JSON text.
func (a *App) issueJSONText(r *Repo, issue *IssueRec) string {
	stamp := a.vault.ISOAtSeq(issue.Seq)
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(issue.ID))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(issue.Number))
	b.WriteString(`,"title":`)
	b.WriteString(jsonQuote(issue.Title))
	b.WriteString(`,"body":`)
	b.WriteString(jsonQuote(issue.Body))
	b.WriteString(`,"state":`)
	b.WriteString(jsonQuote(issue.State))
	b.WriteString(`,"labels":[`)
	for i, name := range issue.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		color := "ededed"
		if rec, ok := r.Labels[name]; ok {
			color = rec.Color
		}
		b.WriteString(`{"name":`)
		b.WriteString(jsonQuote(name))
		b.WriteString(`,"color":`)
		b.WriteString(jsonQuote(color))
		b.WriteByte('}')
	}
	b.WriteString(`],"user":{"login":`)
	b.WriteString(jsonQuote(a.vault.Login))
	b.WriteString(`,"id":1,"type":"User"},"comments":`)
	b.WriteString(strconv.Itoa(len(issue.Comments)))
	b.WriteString(`,"created_at":`)
	b.WriteString(jsonQuote(stamp))
	b.WriteString(`,"updated_at":`)
	b.WriteString(jsonQuote(stamp))
	b.WriteString("}\n")
	return b.String()
}

func (a *App) issueJSON(r *Repo, issue *IssueRec) map[string]any {
	labels := []any{}
	for _, name := range issue.Labels {
		color := "ededed"
		if rec, ok := r.Labels[name]; ok {
			color = rec.Color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	stamp := a.vault.ISOAtSeq(issue.Seq)
	return map[string]any{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": a.vault.Login, "id": 1, "type": "User"},
		"comments":   len(issue.Comments),
		"created_at": stamp,
		"updated_at": stamp,
	}
}

func (a *App) pullJSON(r *Repo, p *PullRec) map[string]any {
	login := a.vault.Login
	var mergedAt any
	if p.Merged {
		mergedAt = a.vault.ISOAtSeq(p.MergedSeq)
	}
	var mergeSHA any
	if p.MergeCommitSHA != "" {
		mergeSHA = p.MergeCommitSHA
	}
	return map[string]any{
		"id":               p.ID,
		"number":           p.Number,
		"title":            p.Title,
		"body":             p.Body,
		"state":            p.State,
		"merged":           p.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head":             map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "label": login + ":" + p.HeadRef},
		"base":             map[string]any{"ref": p.BaseRef, "sha": p.BaseSHA, "label": login + ":" + p.BaseRef},
		"user":             map[string]any{"login": login, "id": 1, "type": "User"},
		"created_at":       a.vault.ISOAtSeq(p.Seq),
		"updated_at":       a.vault.ISOAtSeq(p.Seq),
	}
}

func (a *App) commentJSON(c *CommentRec) map[string]any {
	stamp := a.vault.ISOAtSeq(c.Seq)
	return map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       map[string]any{"login": a.vault.Login, "id": 1, "type": "User"},
		"created_at": stamp,
		"updated_at": stamp,
	}
}

// locateRepo resolves /repos/{owner}/{name}: 404 for a foreign owner or a
// missing name (adopting the login).
func (a *App) locateRepo(owner, name string) *Repo {
	found, ok := a.vault.Repos[name]
	if !ok {
		return nil
	}
	if !a.vault.OwnerMatches(owner) {
		return nil
	}
	a.vault.AdoptLogin(owner)
	return found
}

func textField(body map[string]any, name string) (string, bool) {
	v, ok := body[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func flagField(body map[string]any, name string) (bool, bool) {
	v, ok := body[name]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// handleRest serves one /api/v3 request. Segments are the decoded path
// segments after the /api/v3 prefix.
func (a *App) handleRest(req *Request, res *Response, query map[string]string, segments []string) {
	verb := req.Method

	// The API root and meta answer without a repository.
	if len(segments) == 0 {
		if verb != "GET" {
			res.SendError(404, "Not Found")
			return
		}
		res.SetHeader("x-oauth-scopes", "repo, read:org")
		res.SendJSON(200, map[string]any{
			"current_user_url":               "/api/v3/user",
			"current_user_repositories_url":  "/api/v3/user/repos",
			"repository_url":                 "/api/v3/repos/{owner}/{repo}",
		})
		return
	}
	if len(segments) == 1 && segments[0] == "meta" {
		if verb != "GET" {
			res.SendError(404, "Not Found")
			return
		}
		res.SendJSON(200, map[string]any{"installed_version": restVersion, "github_services_sha": "forge"})
		return
	}

	// Everything below the root needs the token.
	if !isAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return
	}

	if len(segments) == 1 && segments[0] == "user" {
		if verb != "GET" {
			res.SendError(404, "Not Found")
			return
		}
		if !a.vault.HasLogin {
			res.SendError(404, "Not Found")
			return
		}
		res.SendJSON(200, map[string]any{"login": a.vault.Login, "id": 1, "type": "User"})
		return
	}

	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if verb == "GET" {
			a.listUserRepos(req, res, query)
			return
		}
		if verb == "POST" {
			a.createUserRepo(req, res)
			return
		}
		res.SendError(404, "Not Found")
		return
	}

	if len(segments) >= 3 && segments[0] == "repos" {
		owner := segments[1]
		name := segments[2]
		found := a.locateRepo(owner, name)
		if found == nil {
			res.SendError(404, "Not Found")
			return
		}
		a.handleRepo(req, res, query, found, segments[3:])
		return
	}

	res.SendError(404, "Not Found")
}

func (a *App) listUserRepos(req *Request, res *Response, query map[string]string) {
	var every []*Repo
	for _, r := range a.vault.Repos {
		every = append(every, r)
	}
	for i := 1; i < len(every); i++ {
		for j := i; j > 0 && every[j].Seq < every[j-1].Seq; j-- {
			every[j], every[j-1] = every[j-1], every[j]
		}
	}
	pageSize := intQuery(query, "per_page", 30)
	if pageSize > 100 {
		pageSize = 100
	}
	pageNo := intQuery(query, "page", 1)
	page := paginate(every, query)
	items := make([]any, 0, len(page))
	for _, r := range page {
		items = append(items, a.repoJSON(r))
	}
	if pageNo*pageSize < len(every) {
		host := req.Headers["host"]
		if host == "" {
			host = "127.0.0.1"
		}
		base := "http://" + host + "/api/v3/user/repos"
		lastPage := (len(every) + pageSize - 1) / pageSize
		res.SetHeader("link", "<"+base+"?per_page="+strconv.Itoa(pageSize)+"&page="+strconv.Itoa(pageNo+1)+">; rel=\"next\", <"+base+"?per_page="+strconv.Itoa(pageSize)+"&page="+strconv.Itoa(lastPage)+">; rel=\"last\"")
	}
	res.SendJSON(200, items)
}

func (a *App) createUserRepo(req *Request, res *Response) {
	body, ok := readJSONBody(req)
	if !ok {
		res.SendError(400, "Problems parsing JSON")
		return
	}
	name, hasName := textField(body, "name")
	if !hasName || name == "" {
		res.SendError(422, "Repository creation failed: name is required")
		return
	}
	if _, exists := a.vault.Repos[name]; exists {
		res.SendError(422, "Repository creation failed: name already exists on this account")
		return
	}
	desc, _ := textField(body, "description")
	priv, _ := flagField(body, "private")
	created := a.vault.CreateRepo(name, desc, priv)
	res.SendJSONText(201, a.repoJSONText(created))
}

// handleRepo routes under /repos/{owner}/{name}/...
func (a *App) handleRepo(req *Request, res *Response, query map[string]string, repo *Repo, sub []string) {
	verb := req.Method

	if len(sub) == 0 {
		switch verb {
		case "GET":
			res.SendJSONText(200, a.repoJSONText(repo))
		case "PATCH":
			body, ok := readJSONBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			if d, ok := textField(body, "description"); ok {
				repo.Description = d
			}
			if n, ok := textField(body, "name"); ok && n != "" && n != repo.Name {
				if _, exists := a.vault.Repos[n]; !exists {
					delete(a.vault.Repos, repo.Name)
					repo.Name = n
					a.vault.Repos[n] = repo
				}
			}
			if p, ok := flagField(body, "private"); ok {
				repo.Private = p
			}
			if d, ok := textField(body, "default_branch"); ok && d != "" {
				repo.DefaultBranch = d
			}
			res.SendJSONText(200, a.repoJSONText(repo))
		case "DELETE":
			delete(a.vault.Repos, repo.Name)
			res.SendEmpty(204)
		default:
			res.SendError(404, "Not Found")
		}
		return
	}

	head := sub[0]
	tail := sub[1:]

	if head == "branches" {
		if len(tail) == 0 && verb == "GET" {
			var list []any
			var names []string
			for refName, oid := range repo.Refs {
				if !strings.HasPrefix(refName, "refs/heads/") {
					continue
				}
				name := refName[len("refs/heads/"):]
				names = append(names, name)
				sha, _ := a.vault.PeelToCommit(oid)
				list = append(list, map[string]any{"name": name, "commit": map[string]any{"sha": sha}, "protected": false})
			}
			_ = names
			// Sort by name for a stable answer.
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && list[j].(map[string]any)["name"].(string) < list[j-1].(map[string]any)["name"].(string); j-- {
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			res.SendJSON(200, paginate(list, query))
			return
		}
		if len(tail) == 1 && verb == "GET" {
			tip, ok := a.vault.BranchTip(repo, tail[0])
			if !ok {
				res.SendError(404, "Not Found")
				return
			}
			sha, _ := a.vault.PeelToCommit(tip)
			res.SendJSON(200, map[string]any{"name": tail[0], "commit": map[string]any{"sha": sha}, "protected": false})
			return
		}
		res.SendError(404, "Not Found")
		return
	}

	if head == "commits" && len(tail) == 0 && verb == "GET" {
		refName := query["sha"]
		if refName == "" {
			refName = repo.DefaultBranch
		}
		tip, ok := a.vault.ResolveCommit(repo, refName)
		if !ok {
			res.SendJSON(200, []any{})
			return
		}
		var rows []any
		for _, oid := range a.vault.History(tip) {
			info := ParseCommit(a.vault.GetObject(oid).Data)
			author := ParseIdent(info.Author)
			committer := ParseIdent(info.Committer)
			var parents []any
			for _, p := range info.Parents {
				parents = append(parents, map[string]any{"sha": p})
			}
			rows = append(rows, map[string]any{
				"sha": oid,
				"commit": map[string]any{
					"message":   info.Message,
					"author":    map[string]any{"name": author.name, "email": author.email, "date": ISOAt(author.epoch)},
					"committer": map[string]any{"name": committer.name, "email": committer.email, "date": ISOAt(committer.epoch)},
					"tree":      map[string]any{"sha": info.Tree},
				},
				"parents": parents,
			})
		}
		res.SendJSON(200, paginate(rows, query))
		return
	}

	if head == "git" && len(tail) > 0 && tail[0] == "refs" {
		if len(tail) == 1 && verb == "POST" {
			body, ok := readJSONBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			refName, hasRef := textField(body, "ref")
			shaHex, hasSHA := textField(body, "sha")
			if !hasRef || !hasSHA {
				res.SendError(422, "ref and sha are required")
				return
			}
			if _, exists := repo.Refs[refName]; exists {
				res.SendError(422, "Reference already exists")
				return
			}
			if _, ok := a.vault.PeelToCommit(shaHex); !ok {
				res.SendError(422, "sha is not a commit in this repository")
				return
			}
			repo.Refs[refName] = shaHex
			res.SendJSON(201, map[string]any{"ref": refName, "object": map[string]any{"sha": shaHex, "type": "commit"}})
			return
		}
		if len(tail) >= 2 && verb == "GET" {
			refName := "refs/" + strings.Join(tail[1:], "/")
			oid, ok := repo.Refs[refName]
			if !ok {
				res.SendError(404, "Not Found")
				return
			}
			typ := "commit"
			if obj := a.vault.GetObject(oid); obj != nil {
				typ = obj.Type.Name()
			}
			res.SendJSON(200, map[string]any{"ref": refName, "object": map[string]any{"sha": oid, "type": typ}})
			return
		}
		res.SendError(404, "Not Found")
		return
	}

	switch head {
	case "contents":
		a.handleContents(req, res, query, repo, strings.Join(tail, "/"))
	case "labels":
		a.handleLabels(req, res, repo, tail)
	case "issues":
		a.handleIssues(req, res, query, repo, tail)
	case "pulls":
		a.handlePulls(req, res, query, repo, tail)
	default:
		res.SendError(404, "Not Found")
	}
}

func (a *App) handleContents(req *Request, res *Response, query map[string]string, repo *Repo, path string) {
	verb := req.Method
	if verb == "GET" {
		refName := query["ref"]
		if refName == "" {
			refName = repo.DefaultBranch
		}
		commitOID, ok := a.vault.ResolveCommit(repo, refName)
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		found := a.vault.PathAt(commitOID, path)
		if found == nil {
			res.SendError(404, "Not Found")
			return
		}
		if found.Kind == "dir" {
			var entries []any
			for _, child := range found.Entries {
				isDir := child.Mode == "40000" || child.Mode == "040000"
				fullPath := child.Name
				if path != "" {
					fullPath = path + "/" + child.Name
				}
				size := 0
				if !isDir {
					if blob := a.vault.GetObject(child.OID); blob != nil {
						size = len(blob.Data)
					}
				}
				typ := "file"
				if isDir {
					typ = "dir"
				}
				entries = append(entries, map[string]any{
					"type": typ, "name": child.Name, "path": fullPath, "sha": child.OID, "size": size,
				})
			}
			res.SendJSON(200, entries)
			return
		}
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		res.SendJSON(200, map[string]any{
			"type": "file", "name": base, "path": path, "sha": found.OID,
			"size": len(found.Data), "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(found.Data),
		})
		return
	}
	if verb == "PUT" || verb == "DELETE" {
		body, ok := readJSONBody(req)
		if !ok {
			res.SendError(400, "Problems parsing JSON")
			return
		}
		message, _ := textField(body, "message")
		branch, hasBranch := textField(body, "branch")
		if !hasBranch {
			branch = repo.DefaultBranch
		}
		shaHex, hasSHA := textField(body, "sha")
		if verb == "PUT" {
			contentText, hasContent := textField(body, "content")
			if !hasContent {
				res.SendError(422, "content is required")
				return
			}
			cleaned := strings.Map(func(r rune) rune {
				if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
					return -1
				}
				return r
			}, contentText)
			data, err := base64.StdEncoding.DecodeString(cleaned)
			if err != nil {
				res.SendError(422, "content is not valid base64")
				return
			}
			wasThere := a.vault.FileExists(repo, branch, path)
			outcome := a.vault.WriteFile(repo, branch, path, data, message, shaHex, hasSHA)
			if !outcome.OK {
				res.SendError(outcome.Status, outcome.Prob)
				return
			}
			status := 201
			if wasThere {
				status = 200
			}
			base := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				base = path[i+1:]
			}
			res.SendJSON(status, map[string]any{
				"content": map[string]any{"sha": outcome.Blob, "encoding": "base64", "name": base, "path": path},
				"commit":  map[string]any{"sha": outcome.Commit},
			})
			return
		}
		outcome := a.vault.WriteFile(repo, branch, path, nil, message, shaHex, hasSHA)
		if !outcome.OK {
			res.SendError(outcome.Status, outcome.Prob)
			return
		}
		res.SendJSON(200, map[string]any{"content": nil, "commit": map[string]any{"sha": outcome.Commit}})
		return
	}
	res.SendError(404, "Not Found")
}

func (a *App) handleLabels(req *Request, res *Response, repo *Repo, tail []string) {
	verb := req.Method
	if len(tail) == 0 && verb == "GET" {
		var list []any
		for _, rec := range repo.Labels {
			list = append(list, map[string]any{"name": rec.Name, "color": rec.Color})
		}
		res.SendJSON(200, list)
		return
	}
	if len(tail) == 0 && verb == "POST" {
		body, ok := readJSONBody(req)
		if !ok {
			res.SendError(400, "Problems parsing JSON")
			return
		}
		name, hasName := textField(body, "name")
		if !hasName || name == "" {
			res.SendError(422, "name is required")
			return
		}
		if _, exists := repo.Labels[name]; exists {
			res.SendError(422, "Label already exists")
			return
		}
		color, _ := textField(body, "color")
		if color == "" {
			color = "ededed"
		}
		repo.Labels[name] = &LabelRec{Name: name, Color: color}
		res.SendJSON(201, map[string]any{"name": name, "color": color})
		return
	}
	res.SendError(404, "Not Found")
}

// labelNames extracts the label names an issue body names, whether as
// strings or {name} objects.
func labelNames(value any) []string {
	arr, ok := value.([]any)
	if !ok {
		return nil
	}
	var found []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			found = append(found, s)
		} else if m, ok := item.(map[string]any); ok {
			if s, ok := m["name"].(string); ok {
				found = append(found, s)
			}
		}
	}
	return found
}

type issueCreateOutcome struct {
	OK     bool
	Status int
	Prob   string
	Issue  *IssueRec
}

// createIssueChecked is the shared issue creation (REST POST and, through
// the store, GraphQL/MCP).
func (a *App) createIssueChecked(repo *Repo, title, body string, labels []string) issueCreateOutcome {
	if title == "" {
		return issueCreateOutcome{Status: 422, Prob: "title is required"}
	}
	for _, wanted := range labels {
		if _, ok := repo.Labels[wanted]; !ok {
			return issueCreateOutcome{Status: 422, Prob: "unknown label: " + wanted}
		}
	}
	created := a.vault.AddIssue(repo, title, body, labels)
	return issueCreateOutcome{OK: true, Issue: created}
}

func (a *App) handleIssues(req *Request, res *Response, query map[string]string, repo *Repo, tail []string) {
	verb := req.Method
	if len(tail) == 0 {
		if verb == "POST" {
			body, ok := readJSONBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			title, _ := textField(body, "title")
			bodyText, _ := textField(body, "body")
			outcome := a.createIssueChecked(repo, title, bodyText, labelNames(body["labels"]))
			if !outcome.OK {
				res.SendError(outcome.Status, outcome.Prob)
				return
			}
			res.SendJSONText(201, a.issueJSONText(repo, outcome.Issue))
			return
		}
		if verb == "GET" {
			stateFilter := query["state"]
			if stateFilter == "" {
				stateFilter = "open"
			}
			dir := query["direction"]
			if dir == "" {
				dir = "desc"
			}
			var list []*IssueRec
			for _, e := range repo.Issues {
				if stateFilter == "open" && e.State != "open" {
					continue
				}
				if stateFilter == "closed" && e.State != "closed" {
					continue
				}
				list = append(list, e)
			}
			for i := 1; i < len(list); i++ {
				for j := i; j > 0; j-- {
					swap := false
					if dir == "asc" {
						swap = list[j].Seq < list[j-1].Seq
					} else {
						swap = list[j].Seq > list[j-1].Seq
					}
					if !swap {
						break
					}
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			page := paginate(list, query)
			items := make([]any, 0, len(page))
			for _, e := range page {
				items = append(items, a.issueJSON(repo, e))
			}
			res.SendJSON(200, items)
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		res.SendError(404, "Not Found")
		return
	}
	found, ok := repo.Issues[num]
	if len(tail) == 1 {
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		if verb == "GET" {
			res.SendJSONText(200, a.issueJSONText(repo, found))
			return
		}
		if verb == "PATCH" {
			body, ok := readJSONBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			if t, ok := textField(body, "title"); ok {
				found.Title = t
			}
			if b, ok := textField(body, "body"); ok {
				found.Body = b
			}
			if s, ok := textField(body, "state"); ok && (s == "open" || s == "closed") {
				found.State = s
			}
			if _, has := body["labels"]; has {
				wanted := labelNames(body["labels"])
				for _, name := range wanted {
					if _, ok := repo.Labels[name]; !ok {
						res.SendError(422, "unknown label: "+name)
						return
					}
				}
				found.Labels = wanted
			}
			res.SendJSONText(200, a.issueJSONText(repo, found))
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		if verb == "GET" {
			page := paginate(found.Comments, query)
			items := make([]any, 0, len(page))
			for _, c := range page {
				items = append(items, a.commentJSON(c))
			}
			res.SendJSON(200, items)
			return
		}
		if verb == "POST" {
			body, ok := readJSONBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			commentBody, hasBody := textField(body, "body")
			if !hasBody {
				res.SendError(422, "body is required")
				return
			}
			added := a.vault.AddComment(found, commentBody)
			res.SendJSON(201, a.commentJSON(added))
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	res.SendError(404, "Not Found")
}

func (a *App) handlePulls(req *Request, res *Response, query map[string]string, repo *Repo, tail []string) {
	verb := req.Method
	if len(tail) == 0 {
		if verb == "POST" {
			body, ok := readJSONBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			title, _ := textField(body, "title")
			bodyText, _ := textField(body, "body")
			head, _ := textField(body, "head")
			base, hasBase := textField(body, "base")
			if !hasBase {
				base = repo.DefaultBranch
			}
			outcome := a.vault.CreatePull(repo, title, bodyText, head, base)
			if !outcome.OK {
				res.SendError(422, outcome.Prob)
				return
			}
			res.SendJSON(201, a.pullJSON(repo, outcome.Pull))
			return
		}
		if verb == "GET" {
			stateFilter := query["state"]
			if stateFilter == "" {
				stateFilter = "open"
			}
			var list []*PullRec
			for _, e := range repo.Pulls {
				if stateFilter == "open" && e.State != "open" {
					continue
				}
				if stateFilter == "closed" && e.State != "closed" {
					continue
				}
				list = append(list, e)
			}
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && list[j].Seq > list[j-1].Seq; j-- {
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			page := paginate(list, query)
			items := make([]any, 0, len(page))
			for _, e := range page {
				items = append(items, a.pullJSON(repo, e))
			}
			res.SendJSON(200, items)
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		res.SendError(404, "Not Found")
		return
	}
	found, ok := repo.Pulls[num]
	if len(tail) == 1 && verb == "GET" {
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		res.SendJSON(200, a.pullJSON(repo, found))
		return
	}
	if len(tail) == 2 && tail[1] == "files" && verb == "GET" {
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		var rows []any
		for _, f := range a.vault.PullFiles(repo, found) {
			rows = append(rows, map[string]any{
				"filename": f.Path, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions,
				"changes": f.Additions + f.Deletions,
			})
		}
		res.SendJSON(200, paginate(rows, query))
		return
	}
	if len(tail) == 2 && tail[1] == "merge" && verb == "PUT" {
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		body, ok := readJSONBody(req)
		if !ok {
			res.SendError(400, "Problems parsing JSON")
			return
		}
		method, _ := textField(body, "merge_method")
		mergeMethod := "squash"
		if method == "merge" || method == "rebase" {
			mergeMethod = method
		}
		outcome := a.vault.MergePull(repo, found, mergeMethod)
		if !outcome.OK {
			res.SendError(outcome.Status, outcome.Prob)
			return
		}
		res.SendJSON(200, map[string]any{"sha": outcome.SHA, "merged": true, "message": "Pull Request successfully merged"})
		return
	}
	res.SendError(404, "Not Found")
}
