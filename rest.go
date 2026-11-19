// REST v3 API for the github-core-clone-01 Go forge.
// Port of the Node entry's src/rest.ts.
//
// Contract (checks-rest.ts is authoritative):
//   - Bearer auth on every /api/v3 request (MAKEFUN_SHOP_TOKEN; any
//     non-empty Bearer when unset). 401 JSON {message} when
//     missing/wrong. No exceptions.
//   - Every reply: application/json, non-empty X-GitHub-Request-Id.
//     Errors: JSON {message}. Malformed JSON body -> 400. Unknown
//     /api/v3 route -> 404 JSON {message:"Not Found"}.
//   - A repo belongs to the token's login: same name under another
//     owner -> 404. Owner identity is learned (see store.go).
//   - Issues and pull requests share ONE number sequence per repo, from 1.
//   - Merge is squash: one commit on the base branch whose tree is the
//     head tip's tree, parents=[base tip]. 405 when conflicted.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytesRest = 10 * 1024 * 1024

// marshalJSON encodes like Node's JSON.stringify (no HTML escaping,
// no trailing newline); the caller adds "\n" where the Node build does.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func marshalJSONLine(v any) []byte {
	b, err := marshalJSON(v)
	if err != nil {
		return []byte("{\"message\":\"Internal Server Error\"}\n")
	}
	return append(b, '\n')
}

type commentRecord struct {
	ID        int
	Body      string
	UserLogin string
	CreatedAt string
	UpdatedAt string
}

// Issue comments live here (IssueRecord carries no comments field).
var commentsByIssue = make(map[string][]*commentRecord)
var nextCommentID = 1

func authorOf(login string) map[string]any {
	return map[string]any{"name": login, "email": login + "@users.noreply.github.com", "date": nowISO()}
}

// dec decodes one path segment leniently (a bad % sequence stays raw).
func dec(seg string) string {
	if d, err := url.PathUnescape(seg); err == nil {
		return d
	}
	return seg
}

func baseURLOf(req *Request) string {
	host := req.Header("host")
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asNumber(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// readJSONBody parses the request body as a JSON object. Empty body -> {}.
func readJSONBody(req *Request) (map[string]any, bool, bool) {
	text := strings.TrimSpace(string(req.Body))
	if text == "" {
		return map[string]any{}, false, false
	}
	if len(req.Body) > maxBodyBytesRest {
		return nil, false, true
	}
	var parsed any
	dec := json.NewDecoder(bytes.NewReader(req.Body))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, true, false
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, true, false
	}
	return obj, false, false
}

func jsonNum(n json.Number) float64 {
	f, _ := n.Float64()
	return f
}

func restSend(requestID string, status int, body any, extra map[string]string) Response {
	headers := [][2]string{{"x-github-request-id", requestID}}
	for k, v := range extra {
		headers = append(headers, [2]string{k, v})
	}
	if status == 204 {
		return Response{Status: 204, Headers: headers}
	}
	b := marshalJSONLine(body)
	headers = append([][2]string{{"content-type", jsonType}}, headers...)
	return Response{Status: status, Headers: headers, Body: b}
}

func restErr(requestID string, status int, message string) Response {
	return restSend(requestID, status, map[string]any{"message": message}, nil)
}

// restSendText sends a pre-serialized JSON body (trailing newline in text).
func restSendText(requestID string, status int, text []byte) Response {
	return Response{Status: status, Headers: [][2]string{
		{"content-type", jsonType},
		{"x-github-request-id", requestID},
	}, Body: text}
}

// Serialized GET-issue responses, per repo+number. An issue's rendering
// only changes when the issue itself changes (PATCH bumps updated_at),
// when a comment lands (commentVersionOf), or when the learned owner
// changes, so those three values are the cache key. The read workload
// GETs one issue 2,000 times, so this turns 1,999 of those into a Map
// lookup plus a write.
type issueCacheEntry struct {
	buf            []byte
	updatedAt      string
	commentVersion int
	owner          string
}

func cachedIssueJSON(store *Store, req *Request, repo *RepoRecord, issue *IssueRecord) []byte {
	if repo.issueCache == nil {
		repo.issueCache = make(map[int]*issueCacheEntry)
	}
	owner := store.DisplayOwner(repo)
	commentVersion := commentVersionOf(repo, issue.Number)
	if cached, ok := repo.issueCache[issue.Number]; ok &&
		cached.updatedAt == issue.UpdatedAt &&
		cached.commentVersion == commentVersion &&
		cached.owner == owner {
		return cached.buf
	}
	buf := marshalJSONLine(issueJSON(store, req, repo, issue))
	repo.issueCache[issue.Number] = &issueCacheEntry{buf: buf, updatedAt: issue.UpdatedAt, commentVersion: commentVersion, owner: owner}
	return buf
}

// segmentsOf splits a remaining path into decoded segments, dropping empties.
func segmentsOf(rest string) []string {
	var out []string
	for _, s := range strings.Split(rest, "/") {
		if s != "" {
			out = append(out, dec(s))
		}
	}
	return out
}

// pageParams: page>=1, per_page 1..100, default 30.
func pageParams(query url.Values) (page, perPage int) {
	page = 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			page = n
		}
	}
	perPage = 30
	if v := query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

// paginate slices a list into one page; returns the page plus a Link header value (or "").
func paginate(items []any, req *Request, query url.Values) ([]any, string) {
	page, perPage := pageParams(query)
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	link := ""
	if start+perPage < len(items) {
		q := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("page", strconv.Itoa(page+1))
		q.Set("per_page", strconv.Itoa(perPage))
		next := baseURLOf(req) + req.Path + "?" + q.Encode()
		lastPage := (len(items) + perPage - 1) / perPage
		q.Set("page", strconv.Itoa(lastPage))
		last := baseURLOf(req) + req.Path + "?" + q.Encode()
		link = "<" + next + ">; rel=\"next\", <" + last + ">; rel=\"last\""
	}
	return slice, link
}

func linkHeader(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

// ---------------------------------------------------------------------------
// JSON views
// ---------------------------------------------------------------------------

func repoJSON(store *Store, req *Request, repo *RepoRecord) map[string]any {
	owner := store.DisplayOwner(repo)
	base := baseURLOf(req)
	fullName := owner + "/" + repo.Name
	openCount := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openCount++
		}
	}
	return map[string]any{
		"id":                repo.ID,
		"node_id":           toGlobalId("Repository", strconv.Itoa(repo.ID)),
		"name":              repo.Name,
		"full_name":         fullName,
		"owner":             map[string]any{"login": owner, "id": 1000, "type": "User", "html_url": base + "/" + owner, "url": base + "/api/v3/users/" + owner},
		"private":           repo.Private,
		"description":       repo.Description,
		"default_branch":    repo.DefaultBranch,
		"html_url":          base + "/" + owner + "/" + repo.Name,
		"url":               base + "/api/v3/repos/" + owner + "/" + repo.Name,
		"created_at":        repo.CreatedAt,
		"updated_at":        repo.UpdatedAt,
		"has_issues":        true,
		"has_projects":      false,
		"fork":              false,
		"archived":          false,
		"disabled":          false,
		"visibility":        map[bool]string{true: "private", false: "public"}[repo.Private],
		"language":          nil,
		"topics":            []any{},
		"license":           nil,
		"size":              0,
		"stargazers_count":  0,
		"watchers_count":    0,
		"forks_count":       0,
		"open_issues_count": openCount,
	}
}

func labelJSON(label *LabelRecord) map[string]any {
	return map[string]any{"id": 0, "node_id": toGlobalId("Label", label.Name), "name": label.Name, "color": label.Color, "description": label.Description, "default": false}
}

// allComments merges REST comments and GraphQL comments for one issue,
// deduped by body (a REST POST also writes to the GraphQL store, so the
// same comment appears in both; the first occurrence wins). GraphQL
// entries get synthetic numeric ids above 1,000,000.
func allComments(repo *RepoRecord, number int) []*commentRecord {
	key := fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, number)
	restList := commentsByIssue[key]
	out := append([]*commentRecord{}, restList...)
	seen := make(map[string]bool)
	for _, c := range restList {
		seen[c.Body] = true
	}
	gqlList := commentsFor(repo, number)
	for i, g := range gqlList {
		if seen[g.Body] {
			continue
		}
		seen[g.Body] = true
		out = append(out, &commentRecord{
			ID:        1000000 + i,
			Body:      g.Body,
			UserLogin: repo.Owner,
			CreatedAt: g.CreatedAt,
			UpdatedAt: g.CreatedAt,
		})
	}
	return out
}

// commentCount without building the merged list: the common case (no
// comments anywhere) is two Map lookups and no allocation.
func commentCount(repo *RepoRecord, number int) int {
	restList := commentsByIssue[fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, number)]
	gqlList := commentsFor(repo, number)
	if len(restList) == 0 && len(gqlList) == 0 {
		return 0
	}
	return len(allComments(repo, number))
}

func issueJSON(store *Store, req *Request, repo *RepoRecord, issue *IssueRecord) map[string]any {
	owner := store.DisplayOwner(repo)
	base := baseURLOf(req)
	var labels []any
	for _, name := range issue.Labels {
		if l, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJSON(l))
		}
	}
	if labels == nil {
		labels = []any{}
	}
	var closedAt any
	if issue.State == "closed" {
		closedAt = issue.UpdatedAt
	}
	return map[string]any{
		"id":             issue.ID,
		"node_id":        toGlobalId("Issue", strconv.Itoa(issue.ID)),
		"number":         issue.Number,
		"title":          issue.Title,
		"body":           issue.Body,
		"state":          issue.State,
		"labels":         labels,
		"user":           map[string]any{"login": issue.UserLogin, "id": 1000, "type": "User"},
		"assignee":       nil,
		"assignees":      []any{},
		"milestone":      nil,
		"locked":         false,
		"comments":       commentCount(repo, issue.Number),
		"created_at":     issue.CreatedAt,
		"updated_at":     issue.UpdatedAt,
		"closed_at":      closedAt,
		"html_url":       base + "/" + owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		"url":            base + "/api/v3/repos/" + owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		"repository_url": base + "/api/v3/repos/" + owner + "/" + repo.Name,
	}
}

func pullJSON(store *Store, req *Request, repo *RepoRecord, pull *PullRecord) map[string]any {
	owner := store.DisplayOwner(repo)
	base := baseURLOf(req)
	headSha := pull.HeadSha
	if s, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headSha = s
	}
	baseSha := pull.BaseSha
	if s, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseSha = s
	}
	repoMini := map[string]any{"id": repo.ID, "name": repo.Name, "full_name": owner + "/" + repo.Name, "owner": map[string]any{"login": owner, "type": "User"}}
	var mergedAt any
	if pull.Merged {
		mergedAt = pull.UpdatedAt
	}
	var closedAt any
	if pull.State == "closed" {
		closedAt = pull.UpdatedAt
	}
	var mergeCommitSha any
	if pull.MergeCommitSha != "" {
		mergeCommitSha = pull.MergeCommitSha
	}
	return map[string]any{
		"id":               pull.ID,
		"node_id":          toGlobalId("PullRequest", strconv.Itoa(pull.ID)),
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"mergeable":        pull.Mergeable,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"draft":            false,
		"locked":           false,
		"head":             map[string]any{"ref": pull.HeadRef, "sha": headSha, "label": owner + ":" + pull.HeadRef, "repo": repoMini},
		"base":             map[string]any{"ref": pull.BaseRef, "sha": baseSha, "label": owner + ":" + pull.BaseRef, "repo": repoMini},
		"user":             map[string]any{"login": pull.UserLogin, "id": 1000, "type": "User"},
		"assignee":         nil,
		"assignees":        []any{},
		"labels":           []any{},
		"milestone":        nil,
		"created_at":       pull.CreatedAt,
		"updated_at":       pull.UpdatedAt,
		"closed_at":        closedAt,
		"html_url":         base + "/" + owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		"url":              base + "/api/v3/repos/" + owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
		"commits_url":      base + "/api/v3/repos/" + owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number) + "/commits",
	}
}

// ---------------------------------------------------------------------------
// Git helpers (built on git.go primitives only)
// ---------------------------------------------------------------------------

func commitTreeSha(repo *RepoRecord, commitSha string) string {
	obj := getObject(repo, commitSha)
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

type parsedWho struct {
	Name  string
	Email string
	Date  string
}

type commitView struct {
	Tree      string
	Parents   []string
	Author    parsedWho
	Committer parsedWho
	Message   string
}

func commitOf(repo *RepoRecord, sha string) *commitView {
	obj := getObject(repo, sha)
	if obj == nil || obj.Type != "commit" {
		return nil
	}
	parsed := parseCommit(obj.Data)
	return &commitView{
		Tree:      parsed.Tree,
		Parents:   parsed.Parents,
		Author:    parseWho(parsed.Author),
		Committer: parseWho(parsed.Committer),
		Message:   strings.TrimRight(parsed.Message, "\n"),
	}
}

// parseWho: "Name <email> <unix-ts> +0000" -> {name, email, date (ISO)}.
func parseWho(raw string) parsedWho {
	raw = strings.TrimSpace(raw)
	// Find "<email> ts +0000" at the end.
	lt := strings.LastIndex(raw, "<")
	gt := strings.LastIndex(raw, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return parsedWho{Name: raw, Email: "", Date: nowISO()}
	}
	name := strings.TrimSpace(raw[:lt])
	email := raw[lt+1 : gt]
	rest := strings.Fields(strings.TrimSpace(raw[gt+1:]))
	if len(rest) < 2 {
		return parsedWho{Name: name, Email: email, Date: nowISO()}
	}
	ts, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return parsedWho{Name: name, Email: email, Date: nowISO()}
	}
	return parsedWho{Name: name, Email: email, Date: unixToISO(ts)}
}

// lookupPath finds the entry for path in the tree treeSha (or nil).
func lookupPath(repo *RepoRecord, treeSha, path string) *TreeEntry {
	parts := []string{}
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	cur := treeSha
	for i, part := range parts {
		obj := getObject(repo, cur)
		if obj == nil {
			return nil
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return nil
		}
		var entry *TreeEntry
		for j := range entries {
			if entries[j].Name == part {
				entry = &entries[j]
				break
			}
		}
		if entry == nil {
			return nil
		}
		if i == len(parts)-1 {
			return entry
		}
		if entry.Mode == "040000" || entry.Mode == "40000" {
			cur = entry.Sha
		} else {
			return nil
		}
	}
	return nil
}

// resolveRefTip: branch name, else a commit sha, else "".
func resolveRefTip(repo *RepoRecord, ref string) string {
	if ref == "" {
		if s, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			return s
		}
		return ""
	}
	if tip, ok := repo.Refs["refs/heads/"+ref]; ok {
		return tip
	}
	if isHex40(ref) {
		return ref
	}
	return ""
}

// listCommitShas: newest-first commit shas reachable from tip
// (pre-order DFS, first parent first).
func listCommitShas(repo *RepoRecord, tip string) []string {
	var out []string
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		parsed := commitOf(repo, sha)
		if parsed == nil {
			continue
		}
		out = append(out, sha)
		for i := len(parsed.Parents) - 1; i >= 0; i-- {
			stack = append(stack, parsed.Parents[i])
		}
	}
	return out
}

func commitJSON(repo *RepoRecord, sha string) map[string]any {
	parsed := commitOf(repo, sha)
	if parsed == nil {
		return nil
	}
	parents := []any{}
	for _, p := range parsed.Parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	return map[string]any{
		"sha":     sha,
		"node_id": toGlobalId("Commit", sha),
		"commit": map[string]any{
			"message":   parsed.Message,
			"author":    map[string]any{"name": parsed.Author.Name, "email": parsed.Author.Email, "date": parsed.Author.Date},
			"committer": map[string]any{"name": parsed.Committer.Name, "email": parsed.Committer.Email, "date": parsed.Committer.Date},
			"tree":      map[string]any{"sha": parsed.Tree},
		},
		"parents": parents,
		"tree":    map[string]any{"sha": parsed.Tree},
	}
}

// flattenTree flattens a tree to path -> blob sha (recursive).
func flattenTree(repo *RepoRecord, treeSha string, out map[string]string) {
	if treeSha == "" {
		return
	}
	var walk func(sha, prefix string)
	walk = func(sha, prefix string) {
		obj := getObject(repo, sha)
		if obj == nil {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if e.Mode == "040000" || e.Mode == "40000" {
				walk(e.Sha, p)
			} else {
				out[p] = e.Sha
			}
		}
	}
	walk(treeSha, "")
}

type treeChange struct {
	OldSha string // "" when absent
	NewSha string // "" when absent
}

// diffTrees: paths whose blob differs between two trees ("" tree = empty).
func diffTrees(repo *RepoRecord, oldTree, newTree string) map[string]treeChange {
	oldMap := make(map[string]string)
	newMap := make(map[string]string)
	flattenTree(repo, oldTree, oldMap)
	flattenTree(repo, newTree, newMap)
	out := make(map[string]treeChange)
	keys := make(map[string]bool)
	for k := range oldMap {
		keys[k] = true
	}
	for k := range newMap {
		keys[k] = true
	}
	for key := range keys {
		o, oOk := oldMap[key]
		n, nOk := newMap[key]
		if !oOk {
			o = ""
		}
		if !nOk {
			n = ""
		}
		if o != n {
			out[key] = treeChange{OldSha: o, NewSha: n}
		}
	}
	return out
}

func countLines(text string) []string {
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff: line additions/deletions between two blob contents (multiset diff).
func lineDiff(oldText, newText string) (additions, deletions int) {
	oldCounts := make(map[string]int)
	for _, line := range countLines(oldText) {
		oldCounts[line]++
	}
	for _, line := range countLines(newText) {
		if left := oldCounts[line]; left > 0 {
			oldCounts[line] = left - 1
		} else {
			additions++
		}
	}
	for _, left := range oldCounts {
		deletions += left
	}
	return additions, deletions
}

func blobText(repo *RepoRecord, sha string) string {
	if sha == "" {
		return ""
	}
	obj := getObject(repo, sha)
	if obj == nil {
		return ""
	}
	return string(obj.Data)
}

// nextSharedNumber: one shared number sequence for issues and pull
// requests in a repo, from 1.
func nextSharedNumber(repo *RepoRecord) int {
	repo.IssueSeq++
	repo.PullSeq = repo.IssueSeq
	return repo.IssueSeq
}

// recordId: repo id block + number (+1M block for PRs).
func recordID(repo *RepoRecord, number int, isPull bool) int {
	id := repo.ID*2_000_000 + number
	if isPull {
		id += 1_000_000
	}
	return id
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func handleRest(req *Request, store *Store) (Response, bool) {
	path := req.Path
	if path != "/api/v3" && !strings.HasPrefix(path, "/api/v3/") {
		return Response{}, false
	}
	requestID := nextRequestID()
	method := req.Method

	// Auth first: every /api/v3 request needs the bearer token (no exceptions).
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	auth := req.Header("authorization")
	bearer := ""
	if strings.HasPrefix(auth, "Bearer ") {
		bearer = strings.TrimSpace(auth[len("Bearer "):])
	}
	authed := false
	if expected == "" {
		authed = bearer != ""
	} else {
		authed = bearer == expected
	}
	if !authed {
		return restErr(requestID, 401, "Requires authentication"), true
	}

	// Body (only methods that carry one). Malformed JSON -> 400.
	body := map[string]any{}
	if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
		read, malformed, tooLarge := readJSONBody(req)
		if tooLarge {
			return restErr(requestID, 413, "Request body too large"), true
		}
		if malformed || read == nil {
			return restErr(requestID, 400, "Malformed JSON in request body"), true
		}
		body = read
	}

	// Normalise: strip a trailing slash (except the root itself).
	norm := path
	if len(norm) > 1 && strings.HasSuffix(norm, "/") {
		norm = norm[:len(norm)-1]
	}
	tail := ""
	if norm != "/api/v3" {
		tail = norm[len("/api/v3/"):]
	}
	seg := segmentsOf(tail)

	// ---- API root -------------------------------------------------------
	if tail == "" {
		if method != "GET" {
			return restErr(requestID, 404, "Not Found"), true
		}
		base := baseURLOf(req)
		return restSend(requestID, 200, map[string]any{
			"current_user_url":                     base + "/api/v3/user",
			"current_user_authorizations_html_url": base + "/settings/applications",
			"authorizations_url":                   base + "/api/v3/authorizations",
			"code_search_url":                      base + "/api/v3/search/code?q={query}",
			"commit_search_url":                    base + "/api/v3/search/commits?q={query}",
			"emails_url":                           base + "/api/v3/user/emails",
			"emojis_url":                           base + "/api/v3/emojis",
			"events_url":                           base + "/api/v3/events",
			"feeds_url":                            base + "/api/v3/feeds",
			"followers_url":                        base + "/api/v3/user/followers",
			"following_url":                        base + "/api/v3/user/following{/target}",
			"gists_url":                            base + "/api/v3/gists{/gist_id}",
			"hub_url":                              base + "/api/v3/hub",
			"issue_search_url":                     base + "/api/v3/search/issues?q={query}",
			"issues_url":                           base + "/api/v3/issues",
			"keys_url":                             base + "/api/v3/user/keys",
			"label_search_url":                     base + "/api/v3/search/labels?q={query}",
			"notifications_url":                    base + "/api/v3/notifications",
			"organization_url":                     base + "/api/v3/orgs/{org}",
			"organization_repositories_url":        base + "/api/v3/orgs/{org}/repos{?type,page,per_page}",
			"organization_teams_url":               base + "/api/v3/orgs/{org}/teams",
			"public_gists_url":                     base + "/api/v3/gists/public",
			"rate_limit_url":                       base + "/api/v3/rate_limit",
			"repository_url":                       base + "/api/v3/repos/{owner}/{repo}",
			"repository_search_url":                base + "/api/v3/search/repositories?q={query}",
			"current_user_repositories_url":        base + "/api/v3/user/repos{?type,page,per_page}",
			"starred_url":                          base + "/api/v3/user/starred{/owner}{/repo}",
			"starred_gists_url":                    base + "/api/v3/gists/starred",
			"user_url":                             base + "/api/v3/users/{user}",
			"user_organizations_url":               base + "/api/v3/user/orgs",
			"user_repositories_url":                base + "/api/v3/user/repos{?type,page,per_page}",
			"user_search_url":                      base + "/api/v3/search/users?q={query}",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"}), true
	}

	// ---- meta -----------------------------------------------------------
	if tail == "meta" && method == "GET" {
		return restSend(requestID, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": true}, nil), true
	}

	// ---- /user/repos ----------------------------------------------------
	if tail == "user/repos" {
		if method == "GET" {
			repos := store.ListRepos(store.TokenOwner)
			items := make([]any, 0, len(repos))
			for _, r := range repos {
				items = append(items, repoJSON(store, req, r))
			}
			page, link := paginate(items, req, req.Query)
			return restSend(requestID, 200, page, linkHeader(link)), true
		}
		if method == "POST" {
			name := strings.TrimSpace(asString(body["name"]))
			if name == "" || strings.Contains(name, "/") || strings.Contains(name, " ") {
				return restErr(requestID, 422, "Validation Failed: name is required"), true
			}
			if store.RepoByName(name) != nil {
				return restErr(requestID, 422, "Repository creation failed: name already exists on this account"), true
			}
			repo := store.CreateRepo(store.TokenOwner, name, asString(body["description"]))
			if v, ok := body["private"].(bool); ok {
				repo.Private = v
			}
			repo.UpdatedAt = nowISO()
			return restSend(requestID, 201, repoJSON(store, req, repo), nil), true
		}
		return restErr(requestID, 404, "Not Found"), true
	}

	// ---- /repos/{owner}/{repo}[...] -------------------------------------
	if len(seg) >= 3 && seg[0] == "repos" {
		owner := seg[1]
		repoName := seg[2]
		repo := store.ResolveRepo(owner, repoName)
		if repo == nil {
			return restErr(requestID, 404, "Not Found"), true
		}
		rest := seg[3:]
		login := store.DisplayOwner(repo)

		// GET /repos/{o}/{r} | PATCH | DELETE
		if len(rest) == 0 {
			if method == "GET" {
				return restSend(requestID, 200, repoJSON(store, req, repo), nil), true
			}
			if method == "PATCH" {
				if v, ok := body["description"].(string); ok {
					repo.Description = v
				}
				if v, ok := body["private"].(bool); ok {
					repo.Private = v
				}
				repo.UpdatedAt = nowISO()
				return restSend(requestID, 200, repoJSON(store, req, repo), nil), true
			}
			if method == "DELETE" {
				store.DeleteRepo(repo.Owner, repo.Name)
				return restSend(requestID, 204, nil, nil), true
			}
			return restErr(requestID, 404, "Not Found"), true
		}

		// ---- branches -------------------------------------------------
		if rest[0] == "branches" {
			if len(rest) == 1 && method == "GET" {
				list := []any{}
				for ref, sha := range repo.Refs {
					if strings.HasPrefix(ref, "refs/heads/") {
						list = append(list, map[string]any{"name": ref[len("refs/heads/"):], "commit": map[string]any{"sha": sha}, "protected": false})
					}
				}
				return restSend(requestID, 200, list, nil), true
			}
			if len(rest) >= 2 && method == "GET" {
				branch := strings.Join(rest[1:], "/")
				sha, ok := repo.Refs["refs/heads/"+branch]
				if !ok {
					return restErr(requestID, 404, "Branch not found"), true
				}
				return restSend(requestID, 200, map[string]any{"name": branch, "commit": map[string]any{"sha": sha}, "protected": false}, nil), true
			}
			return restErr(requestID, 404, "Not Found"), true
		}

		// ---- commits --------------------------------------------------
		if rest[0] == "commits" {
			if len(rest) == 1 && method == "GET" {
				tip := resolveRefTip(repo, req.Query.Get("sha"))
				if tip == "" {
					return restSend(requestID, 200, []any{}, nil), true
				}
				shas := listCommitShas(repo, tip)
				items := make([]any, 0, len(shas))
				for _, s := range shas {
					items = append(items, s)
				}
				page, link := paginate(items, req, req.Query)
				out := []any{}
				for _, p := range page {
					if item := commitJSON(repo, p.(string)); item != nil {
						out = append(out, item)
					}
				}
				return restSend(requestID, 200, out, linkHeader(link)), true
			}
			if len(rest) == 2 && method == "GET" {
				wanted := rest[1]
				tip, ok := repo.Refs["refs/heads/"+wanted]
				if !ok {
					if isHex40(wanted) {
						tip = wanted
					} else {
						return restErr(requestID, 404, "Not Found"), true
					}
				}
				item := commitJSON(repo, tip)
				if item == nil {
					return restErr(requestID, 404, "Not Found"), true
				}
				return restSend(requestID, 200, item, nil), true
			}
			return restErr(requestID, 404, "Not Found"), true
		}

		// ---- git/refs -------------------------------------------------
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
			if method == "POST" && len(rest) == 2 {
				ref := asString(body["ref"])
				sha := asString(body["sha"])
				if !strings.HasPrefix(ref, "refs/heads/") || !isHex40(sha) {
					return restErr(requestID, 422, "Validation Failed: ref and sha are required"), true
				}
				if _, ok := repo.Refs[ref]; ok {
					return restErr(requestID, 422, "Reference already exists"), true
				}
				obj := getObject(repo, sha)
				if obj == nil || obj.Type != "commit" {
					return restErr(requestID, 422, "Validation Failed: sha is not a commit"), true
				}
				repo.Refs[ref] = sha
				noteReachable(repo, sha)
				base := baseURLOf(req)
				return restSend(requestID, 201, map[string]any{
					"ref":     ref,
					"node_id": toGlobalId("Ref", ref),
					"url":     base + "/api/v3/repos/" + login + "/" + repo.Name + "/git/refs/" + url.PathEscape(ref),
					"object":  map[string]any{"sha": sha, "type": "commit", "url": base + "/api/v3/repos/" + login + "/" + repo.Name + "/git/commits/" + sha},
				}, nil), true
			}
			if method == "GET" {
				base := baseURLOf(req)
				list := []any{}
				for ref, sha := range repo.Refs {
					list = append(list, map[string]any{
						"ref":     ref,
						"node_id": toGlobalId("Ref", ref),
						"url":     base + "/api/v3/repos/" + login + "/" + repo.Name + "/git/refs/" + url.PathEscape(ref),
						"object":  map[string]any{"sha": sha, "type": "commit"},
					})
				}
				if len(rest) == 2 {
					return restSend(requestID, 200, list, nil), true
				}
				wanted := "refs/" + strings.Join(rest[2:], "/")
				for _, item := range list {
					if m, ok := item.(map[string]any); ok && m["ref"] == wanted {
						return restSend(requestID, 200, m, nil), true
					}
				}
				return restErr(requestID, 404, "Not Found"), true
			}
			return restErr(requestID, 404, "Not Found"), true
		}

		// ---- contents -------------------------------------------------
		if rest[0] == "contents" {
			return handleRestContents(req, requestID, store, repo, login, rest[1:], method, body), true
		}

		// ---- labels ---------------------------------------------------
		if rest[0] == "labels" {
			if len(rest) == 1 && method == "GET" {
				list := []any{}
				for _, l := range repo.Labels {
					list = append(list, labelJSON(l))
				}
				return restSend(requestID, 200, list, nil), true
			}
			if len(rest) == 1 && method == "POST" {
				name := strings.TrimSpace(asString(body["name"]))
				if name == "" {
					return restErr(requestID, 422, "Validation Failed: name is required"), true
				}
				if _, ok := repo.Labels[name]; ok {
					return restErr(requestID, 422, "Validation Failed: label already exists"), true
				}
				label := &LabelRecord{Name: name, Color: asString(body["color"]), Description: asString(body["description"])}
				if label.Color == "" {
					label.Color = "ededed"
				}
				repo.Labels[name] = label
				return restSend(requestID, 201, labelJSON(label), nil), true
			}
			return restErr(requestID, 404, "Not Found"), true
		}

		// ---- issues ---------------------------------------------------
		if rest[0] == "issues" {
			return handleRestIssues(req, requestID, store, repo, login, rest, method, body), true
		}

		// ---- pulls ----------------------------------------------------
		if rest[0] == "pulls" {
			return handleRestPulls(req, requestID, store, repo, login, rest, method, body), true
		}

		return restErr(requestID, 404, "Not Found"), true
	}

	// Unknown route under /api/v3.
	return restErr(requestID, 404, "Not Found"), true
}

func handleRestContents(req *Request, requestID string, store *Store, repo *RepoRecord, login string, rest []string, method string, body map[string]any) Response {
	contentPath := strings.Join(rest, "/")
	base := baseURLOf(req)
	if method == "PUT" {
		message := asString(body["message"])
		contentB64, contentIsStr := body["content"].(string)
		if message == "" || !contentIsStr {
			return restErr(requestID, 422, "Validation Failed: message and content are required")
		}
		if contentPath == "" {
			return restErr(requestID, 422, "Validation Failed: path is required")
		}
		branch := asString(body["branch"])
		if branch == "" {
			branch = repo.DefaultBranch
		}
		refName := "refs/heads/" + branch
		tip, tipOk := repo.Refs[refName]
		if !tipOk && asString(body["branch"]) != "" && body["branch"] != repo.DefaultBranch {
			return restErr(requestID, 422, "Validation Failed: branch "+branch+" does not exist")
		}
		var tipTree string
		var hasTipTree bool
		var existing *TreeEntry
		if tipOk {
			if t := commitTreeSha(repo, tip); t != "" {
				tipTree, hasTipTree = t, true
				existing = lookupPath(repo, tipTree, contentPath)
			}
		}
		isUpdate := existing != nil
		if existing != nil {
			givenSha := asString(body["sha"])
			if givenSha == "" {
				return restErr(requestID, 422, "Validation Failed: sha is required to update a file")
			}
			if !strings.EqualFold(givenSha, existing.Sha) {
				return restErr(requestID, 409, "Conflict: sha does not match the current file")
			}
		}
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
				return -1
			}
			return r
		}, contentB64)
		data, _ := base64Decode(cleaned)
		blobSha := storeObject(repo, "blob", data)
		parts := []string{}
		for _, p := range strings.Split(contentPath, "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		newTree := updatePathInTree(repo, tipTree, hasTipTree, parts, blobSha, true, "")
		who := authorOf(store.TokenOwner)
		var parents []string
		if tipOk {
			parents = []string{tip}
		}
		commitSha := createCommit(repo, newTree, parents, message, who["name"].(string), who["email"].(string))
		repo.Refs[refName] = commitSha
		noteReachable(repo, commitSha)
		repo.UpdatedAt = nowISO()
		name := contentPath
		if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
			name = contentPath[idx+1:]
		}
		return restSend(requestID, map[bool]int{true: 200, false: 201}[isUpdate], map[string]any{
			"content": map[string]any{
				"name": name, "path": contentPath, "sha": blobSha, "size": len(data), "type": "file",
				"encoding":     "base64",
				"url":          base + "/api/v3/repos/" + login + "/" + repo.Name + "/contents/" + contentPath,
				"html_url":     base + "/" + login + "/" + repo.Name + "/blob/" + branch + "/" + contentPath,
				"download_url": base + "/api/v3/repos/" + login + "/" + repo.Name + "/contents/" + contentPath,
			},
			"commit": map[string]any{"sha": commitSha, "message": message, "author": who, "committer": who},
		}, nil)
	}
	if method == "GET" {
		tip := resolveRefTip(repo, req.Query.Get("ref"))
		if tip == "" {
			return restErr(requestID, 404, "Not Found")
		}
		treeSha := commitTreeSha(repo, tip)
		if treeSha == "" {
			return restErr(requestID, 404, "Not Found")
		}
		if contentPath == "" {
			obj := getObject(repo, treeSha)
			var entries []TreeEntry
			if obj != nil {
				entries, _ = parseTree(obj.Data)
			}
			items := []any{}
			for _, e := range entries {
				isDir := e.Mode == "040000" || e.Mode == "40000"
				size := 0
				if !isDir {
					if blob := getObject(repo, e.Sha); blob != nil {
						size = len(blob.Data)
					}
				}
				typ := "file"
				if isDir {
					typ = "dir"
				}
				items = append(items, map[string]any{"type": typ, "name": e.Name, "path": e.Name, "sha": e.Sha, "size": size, "url": base + "/api/v3/repos/" + login + "/" + repo.Name + "/contents/" + e.Name})
			}
			return restSend(requestID, 200, items, nil)
		}
		entry := lookupPath(repo, treeSha, contentPath)
		if entry == nil {
			return restErr(requestID, 404, "Not Found")
		}
		name := contentPath
		if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
			name = contentPath[idx+1:]
		}
		if entry.Mode == "040000" || entry.Mode == "40000" {
			obj := getObject(repo, entry.Sha)
			var entries []TreeEntry
			if obj != nil {
				entries, _ = parseTree(obj.Data)
			}
			items := []any{}
			for _, e := range entries {
				isDir := e.Mode == "040000" || e.Mode == "40000"
				size := 0
				if !isDir {
					if blob := getObject(repo, e.Sha); blob != nil {
						size = len(blob.Data)
					}
				}
				typ := "file"
				if isDir {
					typ = "dir"
				}
				items = append(items, map[string]any{"type": typ, "name": e.Name, "path": contentPath + "/" + e.Name, "sha": e.Sha, "size": size, "url": base + "/api/v3/repos/" + login + "/" + repo.Name + "/contents/" + contentPath + "/" + e.Name})
			}
			return restSend(requestID, 200, items, nil)
		}
		blob := getObject(repo, entry.Sha)
		if blob == nil {
			return restErr(requestID, 404, "Not Found")
		}
		return restSend(requestID, 200, map[string]any{
			"name": name, "path": contentPath, "sha": entry.Sha, "size": len(blob.Data), "type": "file",
			"encoding":     "base64",
			"content":      base64Encode(blob.Data),
			"url":          base + "/api/v3/repos/" + login + "/" + repo.Name + "/contents/" + contentPath,
			"html_url":     base + "/" + login + "/" + repo.Name + "/blob/" + repo.DefaultBranch + "/" + contentPath,
			"download_url": base + "/api/v3/repos/" + login + "/" + repo.Name + "/contents/" + contentPath,
		}, nil)
	}
	if method == "DELETE" {
		branch := asString(body["branch"])
		if branch == "" {
			branch = repo.DefaultBranch
		}
		refName := "refs/heads/" + branch
		tip, ok := repo.Refs[refName]
		if !ok {
			return restErr(requestID, 404, "Not Found")
		}
		tipTree := commitTreeSha(repo, tip)
		if tipTree == "" {
			return restErr(requestID, 404, "Not Found")
		}
		existing := lookupPath(repo, tipTree, contentPath)
		if existing == nil {
			return restErr(requestID, 404, "Not Found")
		}
		givenSha := asString(body["sha"])
		if givenSha != "" && !strings.EqualFold(givenSha, existing.Sha) {
			return restErr(requestID, 422, "Validation Failed: sha does not match the current file")
		}
		message := asString(body["message"])
		if message == "" {
			message = "Delete " + contentPath
		}
		parts := []string{}
		for _, p := range strings.Split(contentPath, "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		newTree := updatePathInTree(repo, tipTree, true, parts, "", false, "")
		who := authorOf(store.TokenOwner)
		commitSha := createCommit(repo, newTree, []string{tip}, message, who["name"].(string), who["email"].(string))
		repo.Refs[refName] = commitSha
		noteReachable(repo, commitSha)
		repo.UpdatedAt = nowISO()
		return restSend(requestID, 200, map[string]any{"content": nil, "commit": map[string]any{"sha": commitSha, "message": message, "author": who, "committer": who}}, nil)
	}
	return restErr(requestID, 404, "Not Found")
}

func handleRestIssues(req *Request, requestID string, store *Store, repo *RepoRecord, login string, rest []string, method string, body map[string]any) Response {
	// POST /issues/{n}/comments, GET /issues/{n}/comments
	if len(rest) == 3 && rest[2] == "comments" {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(requestID, 404, "Not Found")
		}
		if _, ok := repo.Issues[number]; !ok {
			return restErr(requestID, 404, "Not Found")
		}
		key := fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, number)
		if method == "POST" {
			text := asString(body["body"])
			if text == "" {
				return restErr(requestID, 422, "Validation Failed: body is required")
			}
			comment := &commentRecord{
				ID:        nextCommentID,
				Body:      text,
				UserLogin: store.TokenOwner,
				CreatedAt: nowISO(),
				UpdatedAt: nowISO(),
			}
			nextCommentID++
			commentsByIssue[key] = append(commentsByIssue[key], comment)
			// Also publish to the GraphQL comment store so issue.comments
			// over GraphQL sees REST-created comments (deduped by body on read).
			addCommentRecord(repo, number, text)
			base := baseURLOf(req)
			return restSend(requestID, 201, map[string]any{
				"id":         comment.ID,
				"node_id":    toGlobalId("IssueComment", strconv.Itoa(comment.ID)),
				"body":       comment.Body,
				"user":       map[string]any{"login": comment.UserLogin},
				"created_at": comment.CreatedAt,
				"updated_at": comment.UpdatedAt,
				"html_url":   base + "/" + login + "/" + repo.Name + "/issues/" + strconv.Itoa(number) + "#issuecomment-" + strconv.Itoa(comment.ID),
				"url":        base + "/api/v3/repos/" + login + "/" + repo.Name + "/issues/comments/" + strconv.Itoa(comment.ID),
			}, nil)
		}
		if method == "GET" {
			base := baseURLOf(req)
			list := allComments(repo, number)
			out := []any{}
			for _, c := range list {
				out = append(out, map[string]any{
					"id":         c.ID,
					"node_id":    toGlobalId("IssueComment", strconv.Itoa(c.ID)),
					"body":       c.Body,
					"user":       map[string]any{"login": c.UserLogin},
					"created_at": c.CreatedAt,
					"updated_at": c.UpdatedAt,
					"html_url":   base + "/" + login + "/" + repo.Name + "/issues/" + strconv.Itoa(number) + "#issuecomment-" + strconv.Itoa(c.ID),
					"url":        base + "/api/v3/repos/" + login + "/" + repo.Name + "/issues/comments/" + strconv.Itoa(c.ID),
				})
			}
			return restSend(requestID, 200, out, nil)
		}
		return restErr(requestID, 404, "Not Found")
	}
	if len(rest) == 1 && method == "GET" {
		list := []*IssueRecord{}
		for _, i := range repo.Issues {
			list = append(list, i)
		}
		state := req.Query.Get("state")
		if state == "" {
			state = "open"
		}
		if state == "open" || state == "closed" {
			var kept []*IssueRecord
			for _, i := range list {
				if i.State == state {
					kept = append(kept, i)
				}
			}
			list = kept
		} else if state != "all" {
			var kept []*IssueRecord
			for _, i := range list {
				if i.State == "open" {
					kept = append(kept, i)
				}
			}
			list = kept
		}
		sortParam := req.Query.Get("sort")
		direction := req.Query.Get("direction")
		if direction == "" {
			direction = "desc"
		}
		if sortParam == "created" || sortParam == "updated" {
			field := "created_at"
			if sortParam == "updated" {
				field = "updated_at"
			}
			sort.Slice(list, func(a, b int) bool {
				var ta, tb string
				if field == "created_at" {
					ta, tb = list[a].CreatedAt, list[b].CreatedAt
				} else {
					ta, tb = list[a].UpdatedAt, list[b].UpdatedAt
				}
				byDate := 0
				if ta < tb {
					byDate = -1
				} else if ta > tb {
					byDate = 1
				}
				if byDate != 0 {
					if direction == "asc" {
						return byDate < 0
					}
					return byDate > 0
				}
				if direction == "asc" {
					return list[a].Number < list[b].Number
				}
				return list[a].Number > list[b].Number
			})
		} else {
			sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
		}
		items := make([]any, 0, len(list))
		for _, i := range list {
			items = append(items, issueJSON(store, req, repo, i))
		}
		page, link := paginate(items, req, req.Query)
		return restSend(requestID, 200, page, linkHeader(link))
	}
	if len(rest) == 1 && method == "POST" {
		title := strings.TrimSpace(asString(body["title"]))
		if title == "" {
			return restErr(requestID, 422, "Validation Failed: title is required")
		}
		var labelNames []string
		if arr, ok := body["labels"].([]any); ok {
			for _, item := range arr {
				var name string
				if s, ok := item.(string); ok {
					name = s
				} else if m, ok := item.(map[string]any); ok {
					name = asString(m["name"])
				}
				if name != "" {
					labelNames = append(labelNames, name)
				}
			}
		}
		for _, name := range labelNames {
			if _, ok := repo.Labels[name]; !ok {
				return restErr(requestID, 422, "Validation Failed: label \""+name+"\" does not exist")
			}
		}
		number := nextSharedNumber(repo)
		issue := &IssueRecord{
			ID:        recordID(repo, number, false),
			Number:    number,
			Title:     title,
			Body:      asString(body["body"]),
			State:     "open",
			Labels:    labelNames,
			UserLogin: store.TokenOwner,
			CreatedAt: nowISO(),
			UpdatedAt: nowISO(),
		}
		repo.Issues[number] = issue
		return restSend(requestID, 201, issueJSON(store, req, repo, issue), nil)
	}
	if len(rest) == 2 {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(requestID, 404, "Not Found")
		}
		issue, ok := repo.Issues[number]
		if !ok {
			return restErr(requestID, 404, "Not Found")
		}
		if method == "GET" {
			return restSendText(requestID, 200, cachedIssueJSON(store, req, repo, issue))
		}
		if method == "PATCH" {
			if v, ok := body["title"].(string); ok && strings.TrimSpace(v) != "" {
				issue.Title = v
			}
			if v, ok := body["body"].(string); ok {
				issue.Body = v
			}
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				issue.State = v
			}
			if arr, ok := body["labels"].([]any); ok {
				var names []string
				for _, item := range arr {
					var name string
					if s, ok := item.(string); ok {
						name = s
					} else if m, ok := item.(map[string]any); ok {
						name = asString(m["name"])
					}
					if name != "" {
						names = append(names, name)
					}
				}
				for _, name := range names {
					if _, ok := repo.Labels[name]; !ok {
						return restErr(requestID, 422, "Validation Failed: label \""+name+"\" does not exist")
					}
				}
				issue.Labels = names
			}
			issue.UpdatedAt = nowISO()
			return restSend(requestID, 200, issueJSON(store, req, repo, issue), nil)
		}
		return restErr(requestID, 404, "Not Found")
	}
	return restErr(requestID, 404, "Not Found")
}

func handleRestPulls(req *Request, requestID string, store *Store, repo *RepoRecord, login string, rest []string, method string, body map[string]any) Response {
	// PUT /pulls/{n}/merge, GET /pulls/{n}/files
	if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(requestID, 404, "Not Found")
		}
		pull, ok := repo.Pulls[number]
		if !ok {
			return restErr(requestID, 404, "Not Found")
		}
		if pull.State != "open" {
			return restErr(requestID, 405, "Pull Request is not open")
		}
		baseTip, baseOk := repo.Refs["refs/heads/"+pull.BaseRef]
		headTip, headOk := repo.Refs["refs/heads/"+pull.HeadRef]
		if !headOk {
			headTip = pull.HeadSha
		}
		if !baseOk || headTip == "" {
			return restErr(requestID, 404, "Not Found")
		}
		mergeBase := findMergeBase(repo, headTip, baseTip)
		if mergeBase == "" {
			return restErr(requestID, 405, "Merge conflict: no common ancestor")
		}
		mbTree := commitTreeSha(repo, mergeBase)
		headTree := commitTreeSha(repo, headTip)
		baseTree := commitTreeSha(repo, baseTip)
		headDiff := diffTrees(repo, mbTree, headTree)
		baseDiff := diffTrees(repo, mbTree, baseTree)
		conflicted := false
		for pathKey, change := range headDiff {
			if other, ok := baseDiff[pathKey]; ok && other.NewSha != change.NewSha {
				conflicted = true
				break
			}
		}
		if conflicted {
			return restErr(requestID, 405, "Merge conflict")
		}
		title := asString(body["commit_title"])
		if title == "" {
			title = pull.Title + " (#" + strconv.Itoa(pull.Number) + ")"
		}
		msgBody := asString(body["commit_message"])
		if msgBody == "" {
			msgBody = pull.Body
		}
		message := title
		if msgBody != "" {
			message = title + "\n\n" + msgBody
		}
		who := authorOf(store.TokenOwner)
		mergeSha := createCommit(repo, headTree, []string{baseTip}, message, who["name"].(string), who["email"].(string))
		repo.Refs["refs/heads/"+pull.BaseRef] = mergeSha
		noteReachable(repo, mergeSha)
		pull.State = "closed"
		pull.Merged = true
		pull.MergeCommitSha = mergeSha
		pull.UpdatedAt = nowISO()
		repo.UpdatedAt = nowISO()
		return restSend(requestID, 200, map[string]any{"sha": mergeSha, "merged": true, "message": "Pull Request successfully merged"}, nil)
	}
	if len(rest) == 3 && rest[2] == "files" && method == "GET" {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(requestID, 404, "Not Found")
		}
		pull, ok := repo.Pulls[number]
		if !ok {
			return restErr(requestID, 404, "Not Found")
		}
		headTip, headOk := repo.Refs["refs/heads/"+pull.HeadRef]
		if !headOk {
			headTip = pull.HeadSha
		}
		baseTip, baseOk := repo.Refs["refs/heads/"+pull.BaseRef]
		if !baseOk {
			baseTip = pull.BaseSha
		}
		mergeBase := findMergeBase(repo, headTip, baseTip)
		var baseTree string
		if mergeBase != "" {
			baseTree = commitTreeSha(repo, mergeBase)
		} else {
			baseTree = commitTreeSha(repo, pull.BaseSha)
		}
		headTree := commitTreeSha(repo, headTip)
		diff := diffTrees(repo, baseTree, headTree)
		files := []any{}
		for filename, change := range diff {
			status := "modified"
			if change.OldSha == "" {
				status = "added"
			} else if change.NewSha == "" {
				status = "removed"
			}
			additions, deletions := lineDiff(blobText(repo, change.OldSha), blobText(repo, change.NewSha))
			files = append(files, map[string]any{"filename": filename, "status": status, "additions": additions, "deletions": deletions, "changes": additions + deletions})
		}
		sort.Slice(files, func(a, b int) bool {
			return files[a].(map[string]any)["filename"].(string) < files[b].(map[string]any)["filename"].(string)
		})
		return restSend(requestID, 200, files, nil)
	}
	if len(rest) == 1 && method == "GET" {
		list := []*PullRecord{}
		for _, p := range repo.Pulls {
			list = append(list, p)
		}
		state := req.Query.Get("state")
		if state == "" {
			state = "open"
		}
		if state == "open" || state == "closed" {
			var kept []*PullRecord
			for _, p := range list {
				if p.State == state {
					kept = append(kept, p)
				}
			}
			list = kept
		} else if state != "all" {
			var kept []*PullRecord
			for _, p := range list {
				if p.State == "open" {
					kept = append(kept, p)
				}
			}
			list = kept
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
		items := make([]any, 0, len(list))
		for _, p := range list {
			items = append(items, pullJSON(store, req, repo, p))
		}
		page, link := paginate(items, req, req.Query)
		return restSend(requestID, 200, page, linkHeader(link))
	}
	if len(rest) == 1 && method == "POST" {
		title := strings.TrimSpace(asString(body["title"]))
		head := asString(body["head"])
		base := asString(body["base"])
		if title == "" || head == "" || base == "" {
			return restErr(requestID, 422, "Validation Failed: title, head and base are required")
		}
		headSha, headOk := repo.Refs["refs/heads/"+head]
		baseSha, baseOk := repo.Refs["refs/heads/"+base]
		if !headOk || !baseOk {
			return restErr(requestID, 404, "Not Found: head or base branch does not exist")
		}
		number := nextSharedNumber(repo)
		pull := &PullRecord{
			ID:        number, // placeholder, fixed below
			Number:    number,
			Title:     title,
			Body:      asString(body["body"]),
			State:     "open",
			Merged:    false,
			Mergeable: true,
			HeadRef:   head,
			BaseRef:   base,
			HeadSha:   headSha,
			BaseSha:   baseSha,
			UserLogin: store.TokenOwner,
			CreatedAt: nowISO(),
			UpdatedAt: nowISO(),
		}
		pull.ID = recordID(repo, number, true)
		repo.Pulls[number] = pull
		return restSend(requestID, 201, pullJSON(store, req, repo, pull), nil)
	}
	if len(rest) == 2 {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(requestID, 404, "Not Found")
		}
		pull, ok := repo.Pulls[number]
		if !ok {
			return restErr(requestID, 404, "Not Found")
		}
		if method == "GET" {
			return restSend(requestID, 200, pullJSON(store, req, repo, pull), nil)
		}
		if method == "PATCH" {
			if v, ok := body["title"].(string); ok && strings.TrimSpace(v) != "" {
				pull.Title = v
			}
			if v, ok := body["body"].(string); ok {
				pull.Body = v
			}
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				pull.State = v
			}
			pull.UpdatedAt = nowISO()
			return restSend(requestID, 200, pullJSON(store, req, repo, pull), nil)
		}
		return restErr(requestID, 404, "Not Found")
	}
	return restErr(requestID, 404, "Not Found")
}
