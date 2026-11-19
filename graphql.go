// GraphQL v4 endpoint (POST /api/graphql) for the github-core-clone-01
// Go forge. Hand-rolled executor for the documents gh and
// github-mcp-server send — NOT a general GraphQL engine. Port of the
// Node entry's src/graphql.ts, including its quirks (the executor is
// the contract: the suite's clients only ever send these shapes).
//
// NUMBERING CONTRACT (agreed with REST): issues and pull requests share
// ONE sequence per repo, repo.IssueSeq, starting at 1.

package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

// repoDatabaseId: numeric repo id: the sequential id store.go assigns,
// falling back to a stable hash when a store never assigned one. This
// is the same value REST returns as `id` / `databaseId`.
func repoDatabaseId(repo *RepoRecord) int {
	if repo.ID > 0 {
		return repo.ID
	}
	key := repo.Owner + "/" + repo.Name
	h := uint32(0x811c9dc5)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 0x01000193
	}
	return int(h%2147483646) + 1
}

// recordIdFor: REST's record-id scheme: repoId * 2M + (1M for PRs) + number.
func recordIdFor(repo *RepoRecord, number int, isPull bool) int {
	id := repoDatabaseId(repo)*2_000_000 + number
	if isPull {
		id += 1_000_000
	}
	return id
}

func repoGlobalId(repo *RepoRecord) string {
	return toGlobalId("Repository", strconv.Itoa(repoDatabaseId(repo)))
}
func issueGlobalId(repo *RepoRecord, issue *IssueRecord) string {
	return toGlobalId("Issue", strconv.Itoa(issue.ID))
}
func pullGlobalId(repo *RepoRecord, pull *PullRecord) string {
	return toGlobalId("PullRequest", strconv.Itoa(pull.ID))
}

func repoURL(repo *RepoRecord) string {
	return "https://github.com/" + repo.Owner + "/" + repo.Name
}

func gqlAuthed(req *Request) bool {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	header := req.Header("authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(header[len("Bearer "):])
	if presented == "" {
		return false
	}
	if token != "" {
		return presented == token
	}
	return true
}

// ---------------------------------------------------------------------------
// Git object helpers (implemented directly on repo.Objects)
// ---------------------------------------------------------------------------

func gqlCommitTreeSha(repo *RepoRecord, commitSha string) string {
	obj := repo.Objects[commitSha]
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	text := string(obj.Data)
	// /^tree ([0-9a-f]{40})/m
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "tree ") && len(line) >= 45 && isHex40(line[5:45]) {
			return line[5:45]
		}
	}
	return ""
}

func gqlCreateCommitObject(repo *RepoRecord, treeSha string, parents []string, actor, message string) string {
	ts := currentUnixSec()
	who := fmt.Sprintf("%s <%s@users.noreply.github.com> %d +0000", actor, actor, ts)
	lines := []string{"tree " + treeSha}
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+who, "committer "+who, "", message, "")
	data := []byte(strings.Join(lines, "\n"))
	sha := sha1Object("commit", data)
	repo.Objects[sha] = &GitObject{Type: "commit", Data: data}
	return sha
}

// flattenTreeGQL flattens a tree object to path -> blob sha (GraphQL's
// copy: only mode "40000" counts as a directory).
func flattenTreeGQL(repo *RepoRecord, treeSha, prefix string, out map[string]string) {
	obj := repo.Objects[treeSha]
	if obj == nil || obj.Type != "tree" {
		return
	}
	buf := obj.Data
	off := 0
	for off < len(buf) {
		sp := indexByte(buf, 0x20, off)
		if sp < 0 {
			break
		}
		mode := string(buf[off:sp])
		nul := indexByte(buf, 0, sp+1)
		if nul < 0 {
			break
		}
		name := string(buf[sp+1 : nul])
		sha := fmt.Sprintf("%x", buf[nul+1:nul+21])
		off = nul + 21
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		if mode == "40000" {
			flattenTreeGQL(repo, sha, path, out)
		} else {
			out[path] = sha
		}
	}
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func pullChangedFiles(repo *RepoRecord, pull *PullRecord) []string {
	baseSha := pull.BaseSha
	if s, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseSha = s
	}
	headSha := pull.HeadSha
	if s, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headSha = s
	}
	baseTree := ""
	if baseSha != "" {
		baseTree = gqlCommitTreeSha(repo, baseSha)
	}
	headTree := ""
	if headSha != "" {
		headTree = gqlCommitTreeSha(repo, headSha)
	}
	base := make(map[string]string)
	head := make(map[string]string)
	if baseTree != "" {
		flattenTreeGQL(repo, baseTree, "", base)
	}
	if headTree != "" {
		flattenTreeGQL(repo, headTree, "", head)
	}
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var changed []string
	for p := range paths {
		if base[p] != head[p] {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed
}

// ---------------------------------------------------------------------------
// Shared record creation / merge (used by GraphQL mutations and MCP tools)
// ---------------------------------------------------------------------------

func createIssueRecord(store *Store, repo *RepoRecord, title, body, actor string) *IssueRecord {
	number := repo.IssueSeq + 1
	repo.IssueSeq = number
	ts := nowISO()
	issue := &IssueRecord{
		ID:        recordIdFor(repo, number, false),
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    []string{},
		UserLogin: actor,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	repo.Issues[number] = issue
	return issue
}

func createPullRecord(repo *RepoRecord, title, body, headRef, baseRef, actor string) *PullRecord {
	headSha, ok := repo.Refs["refs/heads/"+headRef]
	if !ok {
		return nil
	}
	baseSha := repo.Refs["refs/heads/"+baseRef]
	number := repo.IssueSeq + 1
	repo.IssueSeq = number
	repo.PullSeq = repo.IssueSeq
	ts := nowISO()
	pull := &PullRecord{
		ID:        recordIdFor(repo, number, true),
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Merged:    false,
		Mergeable: true,
		HeadRef:   headRef,
		BaseRef:   baseRef,
		HeadSha:   headSha,
		BaseSha:   baseSha,
		UserLogin: actor,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	repo.Pulls[number] = pull
	return pull
}

// squashMergePull: commit on the base branch, tree = head tip tree,
// parents = [base tip].
func squashMergePull(repo *RepoRecord, pull *PullRecord, actor string) (string, string) {
	if pull.Merged {
		return "", "pull request is already merged"
	}
	if pull.State != "open" {
		return "", "pull request is closed"
	}
	baseRef := "refs/heads/" + pull.BaseRef
	headRef := "refs/heads/" + pull.HeadRef
	baseSha, ok1 := repo.Refs[baseRef]
	headSha, ok2 := repo.Refs[headRef]
	if !ok1 || !ok2 {
		return "", "missing branch tip"
	}
	headTree := gqlCommitTreeSha(repo, headSha)
	if headTree == "" {
		return "", "missing head tree"
	}
	sha := gqlCreateCommitObject(repo, headTree, []string{baseSha}, actor, pull.Title+" (#"+strconv.Itoa(pull.Number)+")")
	repo.Refs[baseRef] = sha
	noteReachable(repo, sha)
	pull.Merged = true
	pull.State = "closed"
	pull.MergeCommitSha = sha
	pull.UpdatedAt = nowISO()
	return sha, ""
}

// ---------------------------------------------------------------------------
// Comments (GraphQL addComment)
// ---------------------------------------------------------------------------

type gqlCommentEntry struct {
	ID        string
	Body      string
	CreatedAt string
}

// commentVersionOf is bumped on every comment add (REST or GraphQL):
// the REST issue-JSON cache keys on it.
func commentVersionOf(repo *RepoRecord, issueNumber int) int {
	if repo.gqlCommentVersions == nil {
		return 0
	}
	return repo.gqlCommentVersions[issueNumber]
}

func addCommentRecord(repo *RepoRecord, issueNumber int, body string) *gqlCommentEntry {
	if repo.gqlComments == nil {
		repo.gqlComments = make(map[int][]*gqlCommentEntry)
	}
	list := repo.gqlComments[issueNumber]
	entry := &gqlCommentEntry{
		ID:        toGlobalId("IssueComment", fmt.Sprintf("%s/%s/%d/%d", repo.Owner, repo.Name, issueNumber, len(list)+1)),
		Body:      body,
		CreatedAt: nowISO(),
	}
	repo.gqlComments[issueNumber] = append(list, entry)
	if repo.gqlCommentVersions == nil {
		repo.gqlCommentVersions = make(map[int]int)
	}
	repo.gqlCommentVersions[issueNumber]++
	return entry
}

func commentsFor(repo *RepoRecord, issueNumber int) []*gqlCommentEntry {
	if repo.gqlComments == nil {
		return nil
	}
	return repo.gqlComments[issueNumber]
}

// ---------------------------------------------------------------------------
// REST-shaped objects (for MCP tool results)
// ---------------------------------------------------------------------------

func restRepoObject(store *Store, repo *RepoRecord) map[string]any {
	owner := store.DisplayOwner(repo)
	return map[string]any{
		"id":             repoDatabaseId(repo),
		"node_id":        repoGlobalId(repo),
		"name":           repo.Name,
		"full_name":      owner + "/" + repo.Name,
		"owner":          map[string]any{"login": owner, "id": 1, "type": "User"},
		"private":        false,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"html_url":       repoURL(repo),
		"url":            repoURL(repo),
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.UpdatedAt,
		"has_issues":     true,
	}
}

func restIssueObject(repo *RepoRecord, issue *IssueRecord) map[string]any {
	labels := []any{}
	for _, name := range issue.Labels {
		labels = append(labels, map[string]any{"name": name})
	}
	return map[string]any{
		"id":         issue.ID,
		"node_id":    issueGlobalId(repo, issue),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": issue.UserLogin},
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
		"html_url":   repoURL(repo) + "/issues/" + strconv.Itoa(issue.Number),
		"url":        repoURL(repo) + "/issues/" + strconv.Itoa(issue.Number),
	}
}

func restPullObject(repo *RepoRecord, pull *PullRecord) map[string]any {
	return map[string]any{
		"id":         pull.ID,
		"node_id":    pullGlobalId(repo, pull),
		"number":     pull.Number,
		"title":      pull.Title,
		"body":       pull.Body,
		"state":      pull.State,
		"merged":     pull.Merged,
		"mergeable":  pull.Mergeable,
		"head":       map[string]any{"ref": pull.HeadRef, "sha": pull.HeadSha, "label": repo.Owner + ":" + pull.HeadRef},
		"base":       map[string]any{"ref": pull.BaseRef, "sha": pull.BaseSha, "label": repo.Owner + ":" + pull.BaseRef},
		"user":       map[string]any{"login": pull.UserLogin},
		"created_at": pull.CreatedAt,
		"updated_at": pull.UpdatedAt,
		"html_url":   repoURL(repo) + "/pull/" + strconv.Itoa(pull.Number),
		"url":        repoURL(repo) + "/pull/" + strconv.Itoa(pull.Number),
		"merge_commit_sha": func() any {
			if pull.MergeCommitSha == "" {
				return nil
			}
			return pull.MergeCommitSha
		}(),
	}
}

// ---------------------------------------------------------------------------
// Schema description (introspection)
// ---------------------------------------------------------------------------

var knownFields = map[string][]string{
	"Query":                            {"viewer", "repository", "node", "search", "__type", "__schema"},
	"Mutation":                         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "description", "createdAt", "updatedAt", "isPrivate"},
	"Issue":                            {"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author", "labels", "comments"},
	"PullRequest":                      {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "url", "createdAt", "updatedAt", "author", "files"},
	"User":                             {"login", "id", "name", "url", "avatarUrl"},
	"Ref":                              {"name", "id", "prefix"},
	"Label":                            {"id", "name", "color", "description", "url"},
	"IssueConnection":                  {"nodes", "edges", "totalCount", "pageInfo"},
	"PullRequestConnection":            {"nodes", "edges", "totalCount", "pageInfo"},
	"LabelConnection":                  {"nodes", "edges", "totalCount", "pageInfo"},
	"IssueCommentConnection":           {"nodes", "edges", "totalCount", "pageInfo"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "totalCount", "pageInfo"},
	"IssueEdge":                        {"node", "cursor"},
	"PullRequestEdge":                  {"node", "cursor"},
	"LabelEdge":                        {"node", "cursor"},
	"IssueCommentEdge":                 {"node", "cursor"},
	"PullRequestChangedFileEdge":       {"node", "cursor"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"IssueComment":                     {"id", "body", "author", "createdAt", "updatedAt"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":          {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "clientMutationId"},
	"SearchResultItemConnection":       {"nodes", "edges", "totalCount", "pageInfo"},
	"__Type":                           {"kind", "name", "description", "fields", "enumValues", "inputFields", "interfaces", "possibleTypes", "specifiedByURL"},
	"__Field":                          {"name", "description", "args", "type", "isDeprecated", "deprecationReason"},
	"__EnumValue":                      {"name", "description", "isDeprecated", "deprecationReason"},
	"__InputValue":                     {"name", "description", "type", "defaultValue"},
	"__Schema":                         {"description", "types", "queryType", "mutationType", "subscriptionType", "directives"},
}

var typeKinds = map[string]string{
	"Query": "OBJECT", "Mutation": "OBJECT", "Repository": "OBJECT", "Issue": "OBJECT",
	"PullRequest": "OBJECT", "User": "OBJECT", "Ref": "OBJECT", "Label": "OBJECT",
	"IssueConnection": "OBJECT", "PullRequestConnection": "OBJECT", "LabelConnection": "OBJECT",
	"IssueCommentConnection": "OBJECT", "PullRequestChangedFileConnection": "OBJECT",
	"IssueEdge": "OBJECT", "PullRequestEdge": "OBJECT", "LabelEdge": "OBJECT",
	"IssueCommentEdge": "OBJECT", "PullRequestChangedFileEdge": "OBJECT",
	"PageInfo": "OBJECT", "PullRequestChangedFile": "OBJECT", "IssueComment": "OBJECT",
	"CreateIssuePayload": "OBJECT", "CloseIssuePayload": "OBJECT",
	"CreatePullRequestPayload": "OBJECT", "MergePullRequestPayload": "OBJECT",
	"AddCommentPayload": "OBJECT", "SearchResultItemConnection": "OBJECT",
	"SearchType": "ENUM", "IssueState": "ENUM", "PullRequestState": "ENUM",
	"MergeableState": "ENUM", "OrderDirection": "ENUM", "IssueOrderField": "ENUM",
	"RepositoryPermission": "ENUM", "PullRequestMergeMethod": "ENUM",
	"__Type": "OBJECT", "__Field": "OBJECT", "__EnumValue": "OBJECT",
	"__InputValue": "OBJECT", "__Schema": "OBJECT",
	"String": "SCALAR", "Int": "SCALAR", "Boolean": "SCALAR", "ID": "SCALAR", "DateTime": "SCALAR", "URI": "SCALAR",
}

var enumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "COMMIT", "CODE", "DISCUSSION", "MILESTONE", "PACKAGE", "PROJECT", "WIKI"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

func typeRef(name string) map[string]any {
	kind, ok := typeKinds[name]
	if !ok {
		kind = "OBJECT"
	}
	return map[string]any{"kind": kind, "name": name, "ofType": nil}
}

func introspectionType(name string) map[string]any {
	kind, ok := typeKinds[name]
	if !ok {
		return nil
	}
	fields := knownFields[name]
	t := map[string]any{
		"kind": kind, "name": name, "description": nil,
		"fields": nil, "enumValues": nil, "inputFields": nil,
		"interfaces": []any{}, "possibleTypes": nil, "specifiedByURL": nil,
	}
	if kind == "OBJECT" || kind == "INTERFACE" {
		fl := []any{}
		for _, f := range fields {
			fl = append(fl, map[string]any{"name": f, "description": nil, "args": []any{}, "type": typeRef("String"), "isDeprecated": false, "deprecationReason": nil})
		}
		t["fields"] = fl
	}
	if kind == "ENUM" {
		ev := []any{}
		for _, v := range enumValues[name] {
			ev = append(ev, map[string]any{"name": v, "description": nil, "isDeprecated": false, "deprecationReason": nil})
		}
		t["enumValues"] = ev
	}
	return t
}

// ---------------------------------------------------------------------------
// GraphQL document parser (tokenizer + recursive descent)
// ---------------------------------------------------------------------------

type valueNode struct {
	kind   string // variable|string|int|float|boolean|null|enum|list|object
	str    string
	num    float64
	b      bool
	items  []*valueNode
	fields map[string]*valueNode
	name   string // variable name
}

type selection struct {
	kind          string // field|fragment|spread
	alias         string
	name          string
	args          map[string]*valueNode
	selections    []*selection
	typeCondition string
}

type operation struct {
	typ         string // query|mutation|subscription
	name        string
	varDefaults map[string]*valueNode
	selections  []*selection
}

type gqlToken struct {
	kind  string
	value string
}

func tokenize(src string) []gqlToken {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.' {
			tokens = append(tokens, gqlToken{"...", "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$@", c) >= 0 {
			tokens = append(tokens, gqlToken{string(c), string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					e := src[j+1]
					switch e {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(e)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{"string", out.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			raw := src[i:j]
			kind := "int"
			if strings.Contains(raw, ".") || strings.ContainsAny(raw, "eE") {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind, raw})
			i = j
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(src) && (src[j] >= 'A' && src[j] <= 'Z' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= '0' && src[j] <= '9' || src[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{"name", src[i:j]})
			i = j
			continue
		}
		i++
	}
	tokens = append(tokens, gqlToken{"EOF", ""})
	return tokens
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind string) (gqlToken, error) {
	t := p.next()
	if t.kind != kind {
		return t, fmt.Errorf("expected %s, got %s %s", kind, t.kind, t.value)
	}
	return t, nil
}
func (p *gqlParser) accept(kind string) bool {
	if p.peek().kind == kind {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() ([]*operation, map[string][]*selection, error) {
	var operations []*operation
	fragments := make(map[string][]*selection)
	for p.peek().kind != "EOF" {
		if p.peek().kind == "name" && p.peek().value == "fragment" {
			p.next()
			nameTok, err := p.expect("name")
			if err != nil {
				return nil, nil, err
			}
			if _, err := p.expect("name"); err != nil { // 'on'
				return nil, nil, err
			}
			if _, err := p.expect("name"); err != nil { // type condition (ignored)
				return nil, nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[nameTok.value] = sels
		} else {
			op, err := p.parseOperation()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, op)
		}
	}
	return operations, fragments, nil
}

func (p *gqlParser) parseOperation() (*operation, error) {
	op := &operation{typ: "query", varDefaults: make(map[string]*valueNode)}
	t := p.peek()
	if t.kind == "name" && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
		op.typ = p.next().value
		if p.peek().kind == "name" {
			op.name = p.next().value
		}
	}
	if p.peek().kind == "(" {
		p.next()
		for p.peek().kind != ")" {
			if _, err := p.expect("$"); err != nil {
				return nil, err
			}
			vname, err := p.expect("name")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			// skip type tokens (names, !, [, ])
			for p.peek().kind == "name" || p.peek().kind == "!" || p.peek().kind == "[" || p.peek().kind == "]" {
				p.next()
			}
			if p.accept("=") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				op.varDefaults[vname.value] = v
			}
			if p.peek().kind == "EOF" || p.peek().kind == ")" {
				break
			}
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sels
	return op, nil
}

func (p *gqlParser) parseSelectionSet() ([]*selection, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	var sels []*selection
	for p.peek().kind != "}" {
		if p.peek().kind == "EOF" {
			return nil, fmt.Errorf("unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if _, err := p.expect("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*selection, error) {
	if p.peek().kind == "..." {
		p.next()
		if p.peek().kind == "name" && p.peek().value == "on" {
			p.next()
			cond, err := p.expect("name")
			if err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &selection{kind: "fragment", typeCondition: cond.value, selections: sels}, nil
		}
		if p.peek().kind == "name" {
			return &selection{kind: "spread", name: p.next().value}, nil
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &selection{kind: "fragment", selections: sels}, nil
	}
	alias := ""
	nameTok, err := p.expect("name")
	if err != nil {
		return nil, err
	}
	name := nameTok.value
	if p.accept(":") {
		alias = name
		nameTok, err = p.expect("name")
		if err != nil {
			return nil, err
		}
		name = nameTok.value
	}
	args := make(map[string]*valueNode)
	if p.peek().kind == "(" {
		p.next()
		for p.peek().kind != ")" {
			aname, err := p.expect("name")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[aname.value] = v
			if p.peek().kind == "EOF" {
				break
			}
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	var sels []*selection
	if p.peek().kind == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &selection{kind: "field", alias: alias, name: name, args: args, selections: sels}, nil
}

func (p *gqlParser) parseValue() (*valueNode, error) {
	t := p.next()
	switch t.kind {
	case "$":
		name, err := p.expect("name")
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: "variable", name: name.value}, nil
	case "string":
		return &valueNode{kind: "string", str: t.value}, nil
	case "int":
		n, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: "int", num: n}, nil
	case "float":
		n, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: "float", num: n}, nil
	case "name":
		switch t.value {
		case "true":
			return &valueNode{kind: "boolean", b: true}, nil
		case "false":
			return &valueNode{kind: "boolean", b: false}, nil
		case "null":
			return &valueNode{kind: "null"}, nil
		default:
			return &valueNode{kind: "enum", str: t.value}, nil
		}
	case "[":
		var items []*valueNode
		for p.peek().kind != "]" {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
			if p.peek().kind == "EOF" {
				break
			}
		}
		if _, err := p.expect("]"); err != nil {
			return nil, err
		}
		return &valueNode{kind: "list", items: items}, nil
	case "{":
		fields := make(map[string]*valueNode)
		for p.peek().kind != "}" {
			fname, err := p.expect("name")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields[fname.value] = v
			if p.peek().kind == "EOF" {
				break
			}
		}
		if _, err := p.expect("}"); err != nil {
			return nil, err
		}
		return &valueNode{kind: "object", fields: fields}, nil
	}
	return nil, fmt.Errorf("unexpected value token %s %s", t.kind, t.value)
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type execCtx struct {
	store     *Store
	vars      map[string]any
	errors    []map[string]any
	fragments map[string][]*selection
}

func resolveValue(node *valueNode, ctx *execCtx) any {
	switch node.kind {
	case "variable":
		if v, ok := ctx.vars[node.name]; ok {
			return v
		}
		return nil
	case "string", "enum":
		return node.str
	case "int", "float":
		return node.num
	case "boolean":
		return node.b
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(node.items))
		for _, i := range node.items {
			out = append(out, resolveValue(i, ctx))
		}
		return out
	case "object":
		out := make(map[string]any)
		for k, v := range node.fields {
			out[k] = resolveValue(v, ctx)
		}
		return out
	}
	return nil
}

func resolveArgs(sel *selection, ctx *execCtx) map[string]any {
	out := make(map[string]any)
	for k, v := range sel.args {
		out[k] = resolveValue(v, ctx)
	}
	return out
}

type resolved struct {
	v  any
	t  string // resolved type name (for recursion)
	nt string // node type (for connections)
}

type gqlConnection struct {
	isConnection bool
	nodes        []any
	totalCount   int
	pageInfo     map[string]any
	nodeType     string
}

func cursorFor(n int) string {
	return base64Encode([]byte("cursor:" + strconv.Itoa(n)))
}

func cursorNumber(cursor any) (int, bool) {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return 0, false
	}
	b, err := base64Decode(s)
	if err != nil {
		return 0, false
	}
	if !strings.HasPrefix(string(b), "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(string(b)[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func gqlPaginateIssues(sorted []*IssueRecord, args map[string]any) *gqlConnection {
	first, hasFirst := toInt(args["first"])
	afterNum, hasAfter := cursorNumber(args["after"])
	start := 0
	if hasAfter {
		idx := -1
		for i, x := range sorted {
			if x.Number == afterNum {
				idx = i
				break
			}
		}
		if idx >= 0 {
			start = idx + 1
		} else {
			start = len(sorted)
		}
	}
	var slice []*IssueRecord
	if hasFirst {
		end := start + first
		if end > len(sorted) {
			end = len(sorted)
		}
		if start > len(sorted) {
			start = len(sorted)
		}
		slice = sorted[start:end]
	} else {
		slice = sorted[start:]
	}
	var endCursor, startCursor any
	if len(slice) > 0 {
		endCursor = cursorFor(slice[len(slice)-1].Number)
		startCursor = cursorFor(slice[0].Number)
	}
	nodes := make([]any, 0, len(slice))
	for _, i := range slice {
		nodes = append(nodes, i)
	}
	return &gqlConnection{
		isConnection: true, nodes: nodes, totalCount: len(sorted),
		pageInfo: map[string]any{
			"hasNextPage": start+len(slice) < len(sorted), "hasPreviousPage": start > 0,
			"startCursor": startCursor, "endCursor": endCursor,
		},
		nodeType: "Issue",
	}
}

func gqlPaginatePulls(sorted []*PullRecord, args map[string]any) *gqlConnection {
	first, hasFirst := toInt(args["first"])
	afterNum, hasAfter := cursorNumber(args["after"])
	start := 0
	if hasAfter {
		idx := -1
		for i, x := range sorted {
			if x.Number == afterNum {
				idx = i
				break
			}
		}
		if idx >= 0 {
			start = idx + 1
		} else {
			start = len(sorted)
		}
	}
	var slice []*PullRecord
	if hasFirst {
		end := start + first
		if end > len(sorted) {
			end = len(sorted)
		}
		if start > len(sorted) {
			start = len(sorted)
		}
		slice = sorted[start:end]
	} else {
		slice = sorted[start:]
	}
	var endCursor, startCursor any
	if len(slice) > 0 {
		endCursor = cursorFor(slice[len(slice)-1].Number)
		startCursor = cursorFor(slice[0].Number)
	}
	nodes := make([]any, 0, len(slice))
	for _, p := range slice {
		nodes = append(nodes, p)
	}
	return &gqlConnection{
		isConnection: true, nodes: nodes, totalCount: len(sorted),
		pageInfo: map[string]any{
			"hasNextPage": start+len(slice) < len(sorted), "hasPreviousPage": start > 0,
			"startCursor": startCursor, "endCursor": endCursor,
		},
		nodeType: "PullRequest",
	}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case jsonNumber:
		f, _ := strconv.ParseFloat(string(n), 64)
		return int(f), true
	}
	return 0, false
}

type jsonNumber string

func sortedIssues(repo *RepoRecord, args map[string]any) []*IssueRecord {
	var list []*IssueRecord
	for _, i := range repo.Issues {
		list = append(list, i)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := make(map[string]bool)
		for _, s := range states {
			wanted[fmt.Sprint(s)] = true
		}
		var kept []*IssueRecord
		for _, i := range list {
			st := "CLOSED"
			if i.State == "open" {
				st = "OPEN"
			}
			if wanted[st] {
				kept = append(kept, i)
			}
		}
		list = kept
	}
	orderBy, _ := args["orderBy"].(map[string]any)
	field := "CREATED_AT"
	if v, ok := orderBy["field"]; ok {
		field = fmt.Sprint(v)
	}
	dir := "ASC"
	if v, ok := orderBy["direction"]; ok {
		dir = fmt.Sprint(v)
	}
	cmp := func(a, b *IssueRecord) int {
		if field == "CREATED_AT" || field == "UPDATED_AT" {
			var ta, tb string
			if field == "CREATED_AT" {
				ta, tb = a.CreatedAt, b.CreatedAt
			} else {
				ta, tb = a.UpdatedAt, b.UpdatedAt
			}
			if ta != tb {
				if ta < tb {
					return -1
				}
				return 1
			}
		}
		return a.Number - b.Number
	}
	sort.Slice(list, func(a, b int) bool {
		c := cmp(list[a], list[b])
		if dir == "DESC" {
			return c > 0
		}
		return c < 0
	})
	return list
}

func sortedPulls(repo *RepoRecord, args map[string]any) []*PullRecord {
	var list []*PullRecord
	for _, p := range repo.Pulls {
		list = append(list, p)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := make(map[string]bool)
		for _, s := range states {
			wanted[fmt.Sprint(s)] = true
		}
		var kept []*PullRecord
		for _, p := range list {
			st := "CLOSED"
			if p.Merged {
				st = "MERGED"
			} else if p.State == "open" {
				st = "OPEN"
			}
			if wanted[st] {
				kept = append(kept, p)
			}
		}
		list = kept
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
	return list
}

func lookupRepoByGlobalId(store *Store, globalId any) *RepoRecord {
	s, ok := globalId.(string)
	if !ok {
		return nil
	}
	typ, id, ok := fromGlobalId(s)
	if ok && typ == "Repository" {
		if n, err := strconv.Atoi(id); err == nil && n > 0 {
			for _, repo := range store.Repos {
				if repoDatabaseId(repo) == n {
					return repo
				}
			}
			return nil
		}
		parts := strings.SplitN(id, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return store.ResolveRepo(parts[0], parts[1])
		}
	}
	if strings.Contains(s, "/") {
		parts := strings.SplitN(s, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return store.ResolveRepo(parts[0], parts[1])
		}
	}
	return nil
}

type nodeRef struct {
	typename string
	value    any // *RepoRecord, or map with __repo/__issue/__pull
}

// parseNodeId mirrors the Node build: numeric-id form
// (Repository:<repoId>, Issue/PullRequest:<repoId*2M + (1M if PR) + number>)
// or the legacy "owner/repo[/number]" form.
func parseNodeId(store *Store, globalId any) *nodeRef {
	s, ok := globalId.(string)
	if !ok {
		return nil
	}
	typ, id, ok := fromGlobalId(s)
	if !ok {
		return nil
	}
	if n, err := strconv.Atoi(id); err == nil && n > 0 {
		switch typ {
		case "Repository":
			for _, repo := range store.Repos {
				if repoDatabaseId(repo) == n {
					return &nodeRef{typename: "Repository", value: repo}
				}
			}
			return nil
		case "Issue", "PullRequest":
			repoId := n / 2_000_000
			rem := n % 2_000_000
			isPull := rem >= 1_000_000
			number := rem
			if isPull {
				number = rem - 1_000_000
			}
			if (typ == "PullRequest") != isPull {
				return nil
			}
			for _, repo := range store.Repos {
				if repoDatabaseId(repo) != repoId {
					continue
				}
				if typ == "Issue" {
					issue, ok := repo.Issues[number]
					if !ok {
						return nil
					}
					return &nodeRef{typename: "Issue", value: map[string]any{"__repo": repo, "__issue": issue}}
				}
				pull, ok := repo.Pulls[number]
				if !ok {
					return nil
				}
				return &nodeRef{typename: "PullRequest", value: map[string]any{"__repo": repo, "__pull": pull}}
			}
			return nil
		}
		return nil
	}
	parts := strings.Split(id, "/")
	if typ == "Repository" && len(parts) == 2 {
		repo := store.ResolveRepo(parts[0], parts[1])
		if repo == nil {
			return nil
		}
		return &nodeRef{typename: "Repository", value: repo}
	}
	if typ == "Issue" && len(parts) == 3 {
		repo := store.ResolveRepo(parts[0], parts[1])
		if repo == nil {
			return nil
		}
		num, _ := strconv.Atoi(parts[2])
		issue, ok := repo.Issues[num]
		if !ok {
			return nil
		}
		return &nodeRef{typename: "Issue", value: map[string]any{"__repo": repo, "__issue": issue}}
	}
	if typ == "PullRequest" && len(parts) == 3 {
		repo := store.ResolveRepo(parts[0], parts[1])
		if repo == nil {
			return nil
		}
		num, _ := strconv.Atoi(parts[2])
		pull, ok := repo.Pulls[num]
		if !ok {
			return nil
		}
		return &nodeRef{typename: "PullRequest", value: map[string]any{"__repo": repo, "__pull": pull}}
	}
	return nil
}

func unwrapIssue(v any) (*RepoRecord, *IssueRecord, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, false
	}
	issue, ok := m["__issue"].(*IssueRecord)
	if !ok {
		return nil, nil, false
	}
	repo, _ := m["__repo"].(*RepoRecord)
	return repo, issue, true
}

func unwrapPull(v any) (*RepoRecord, *PullRecord, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, false
	}
	pull, ok := m["__pull"].(*PullRecord)
	if !ok {
		return nil, nil, false
	}
	repo, _ := m["__repo"].(*RepoRecord)
	return repo, pull, true
}

func issueState(issue *IssueRecord) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(pull *PullRecord) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func wrapIssue(repo *RepoRecord, issue *IssueRecord) map[string]any {
	return map[string]any{"__repo": repo, "__issue": issue}
}

func wrapPull(repo *RepoRecord, pull *PullRecord) map[string]any {
	return map[string]any{"__repo": repo, "__pull": pull}
}

func fieldValue(typeName string, src any, field string, args map[string]any, ctx *execCtx) resolved {
	store := ctx.store
	actor := store.TokenOwner
	s, _ := src.(map[string]any)

	switch typeName {
	case "Query":
		switch field {
		case "viewer":
			return resolved{v: map[string]any{"login": actor}, t: "User"}
		case "repository":
			repo := store.ResolveRepo(gqlStr(args["owner"]), gqlStr(args["name"]))
			if repo == nil {
				return resolved{v: nil, t: "Repository"}
			}
			return resolved{v: repo, t: "Repository"}
		case "node":
			found := parseNodeId(store, args["id"])
			if found == nil {
				return resolved{v: nil}
			}
			return resolved{v: map[string]any{"__typename": found.typename, "__src": found.value}, t: found.typename}
		case "search":
			q := strings.ToLower(gqlStr(args["query"]))
			type match struct {
				repo  *RepoRecord
				issue *IssueRecord
			}
			var matches []match
			for _, repo := range store.Repos {
				for _, issue := range repo.Issues {
					if q == "" || strings.Contains(strings.ToLower(issue.Title), q) || strings.Contains(strings.ToLower(issue.Body), q) {
						matches = append(matches, match{repo, issue})
					}
				}
			}
			sort.Slice(matches, func(a, b int) bool { return matches[a].issue.Number > matches[b].issue.Number })
			first := 10
			if n, ok := toInt(args["first"]); ok {
				first = n
			}
			if first > len(matches) {
				first = len(matches)
			}
			nodes := make([]any, 0, first)
			for _, m := range matches[:first] {
				nodes = append(nodes, wrapIssue(m.repo, m.issue))
			}
			conn := &gqlConnection{
				isConnection: true, nodes: nodes, totalCount: len(matches),
				pageInfo: map[string]any{"hasNextPage": len(matches) > len(nodes), "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
				nodeType: "Issue",
			}
			return resolved{v: conn, t: "SearchResultItemConnection", nt: "Issue"}
		case "__type":
			t := introspectionType(gqlStr(args["name"]))
			return resolved{v: t, t: "__Type"}
		case "__schema":
			return resolved{v: map[string]any{}, t: "__Schema"}
		}
	case "Mutation":
		switch field {
		case "createIssue":
			input, _ := args["input"].(map[string]any)
			repo := lookupRepoByGlobalId(store, input["repositoryId"])
			if repo == nil {
				ctx.errors = append(ctx.errors, map[string]any{"message": "createIssue: repository not found for id " + gqlStr(input["repositoryId"])})
				return resolved{v: map[string]any{"issue": nil}, t: "CreateIssuePayload"}
			}
			issue := createIssueRecord(store, repo, gqlStr(input["title"]), gqlStr(input["body"]), actor)
			return resolved{v: map[string]any{"issue": wrapIssue(repo, issue)}, t: "CreateIssuePayload"}
		case "closeIssue":
			input, _ := args["input"].(map[string]any)
			found := parseNodeId(store, input["issueId"])
			var done map[string]any
			if found != nil && found.typename == "Issue" {
				if repo, issue, ok := unwrapIssue(found.value); ok {
					issue.State = "closed"
					issue.UpdatedAt = nowISO()
					done = wrapIssue(repo, issue)
				}
			}
			if done == nil {
				ctx.errors = append(ctx.errors, map[string]any{"message": "closeIssue: issue not found for id " + gqlStr(input["issueId"])})
			}
			return resolved{v: map[string]any{"issue": done}, t: "CloseIssuePayload"}
		case "createPullRequest":
			input, _ := args["input"].(map[string]any)
			repo := lookupRepoByGlobalId(store, input["repositoryId"])
			if repo == nil {
				ctx.errors = append(ctx.errors, map[string]any{"message": "createPullRequest: repository not found for id " + gqlStr(input["repositoryId"])})
				return resolved{v: map[string]any{"pullRequest": nil}, t: "CreatePullRequestPayload"}
			}
			pull := createPullRecord(repo, gqlStr(input["title"]), gqlStr(input["body"]), gqlStr(input["headRefName"]), gqlStr(input["baseRefName"]), actor)
			if pull == nil {
				ctx.errors = append(ctx.errors, map[string]any{"message": "createPullRequest: head branch " + gqlStr(input["headRefName"]) + " does not exist"})
				return resolved{v: map[string]any{"pullRequest": nil}, t: "CreatePullRequestPayload"}
			}
			return resolved{v: map[string]any{"pullRequest": wrapPull(repo, pull)}, t: "CreatePullRequestPayload"}
		case "mergePullRequest":
			input, _ := args["input"].(map[string]any)
			found := parseNodeId(store, input["pullRequestId"])
			var done map[string]any
			if found != nil && found.typename == "PullRequest" {
				if repo, pull, ok := unwrapPull(found.value); ok {
					sha, errStr := squashMergePull(repo, pull, actor)
					if errStr != "" {
						ctx.errors = append(ctx.errors, map[string]any{"message": "mergePullRequest: " + errStr})
					} else {
						_ = sha
						done = wrapPull(repo, pull)
					}
				}
			}
			if done == nil && len(ctx.errors) == 0 {
				ctx.errors = append(ctx.errors, map[string]any{"message": "mergePullRequest: pull request not found for id " + gqlStr(input["pullRequestId"])})
			}
			return resolved{v: map[string]any{"pullRequest": done}, t: "MergePullRequestPayload"}
		case "addComment":
			input, _ := args["input"].(map[string]any)
			found := parseNodeId(store, input["subjectId"])
			var node *gqlCommentEntry
			if found != nil && found.typename == "Issue" {
				if repo, issue, ok := unwrapIssue(found.value); ok {
					node = addCommentRecord(repo, issue.Number, gqlStr(input["body"]))
				}
			}
			if node == nil {
				ctx.errors = append(ctx.errors, map[string]any{"message": "addComment: subject not found for id " + gqlStr(input["subjectId"])})
				return resolved{v: map[string]any{"commentEdge": nil}, t: "AddCommentPayload"}
			}
			return resolved{v: map[string]any{"commentEdge": map[string]any{"node": node}}, t: "AddCommentPayload"}
		}
	case "Repository":
		repo, _ := src.(*RepoRecord)
		if repo == nil {
			return resolved{v: nil}
		}
		switch field {
		case "id":
			return resolved{v: repoGlobalId(repo)}
		case "databaseId":
			return resolved{v: repoDatabaseId(repo)}
		case "name":
			return resolved{v: repo.Name}
		case "nameWithOwner":
			return resolved{v: repo.Owner + "/" + repo.Name}
		case "description":
			return resolved{v: repo.Description}
		case "url":
			return resolved{v: repoURL(repo)}
		case "createdAt":
			return resolved{v: repo.CreatedAt}
		case "updatedAt":
			return resolved{v: repo.UpdatedAt}
		case "isPrivate":
			return resolved{v: false}
		case "viewerPermission":
			return resolved{v: "ADMIN"}
		case "hasIssuesEnabled":
			return resolved{v: true}
		case "owner":
			return resolved{v: map[string]any{"login": store.DisplayOwner(repo)}, t: "User"}
		case "defaultBranchRef":
			sha, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if !ok {
				return resolved{v: nil}
			}
			_ = sha
			return resolved{v: map[string]any{"name": repo.DefaultBranch, "__repo": repo}, t: "Ref"}
		case "issue":
			num, _ := toInt(args["number"])
			issue, ok := repo.Issues[num]
			if !ok {
				return resolved{v: nil, t: "Issue"}
			}
			return resolved{v: wrapIssue(repo, issue), t: "Issue"}
		case "issues":
			sorted := sortedIssues(repo, args)
			conn := gqlPaginateIssues(sorted, args)
			wrapped := make([]any, 0, len(conn.nodes))
			for _, n := range conn.nodes {
				wrapped = append(wrapped, wrapIssue(repo, n.(*IssueRecord)))
			}
			conn.nodes = wrapped
			return resolved{v: conn, t: "IssueConnection", nt: "Issue"}
		case "pullRequest":
			num, _ := toInt(args["number"])
			pull, ok := repo.Pulls[num]
			if !ok {
				return resolved{v: nil, t: "PullRequest"}
			}
			return resolved{v: wrapPull(repo, pull), t: "PullRequest"}
		case "pullRequests":
			sorted := sortedPulls(repo, args)
			conn := gqlPaginatePulls(sorted, args)
			wrapped := make([]any, 0, len(conn.nodes))
			for _, n := range conn.nodes {
				wrapped = append(wrapped, wrapPull(repo, n.(*PullRecord)))
			}
			conn.nodes = wrapped
			return resolved{v: conn, t: "PullRequestConnection", nt: "PullRequest"}
		}
	case "Issue":
		repo, issue, ok := unwrapIssue(src)
		if !ok {
			return resolved{v: nil}
		}
		switch field {
		case "id":
			return resolved{v: issueGlobalId(repo, issue)}
		case "number":
			return resolved{v: issue.Number}
		case "title":
			return resolved{v: issue.Title}
		case "state":
			return resolved{v: issueState(issue)}
		case "body":
			return resolved{v: issue.Body}
		case "url":
			return resolved{v: repoURL(repo) + "/issues/" + strconv.Itoa(issue.Number)}
		case "createdAt":
			return resolved{v: issue.CreatedAt}
		case "updatedAt":
			return resolved{v: issue.UpdatedAt}
		case "author":
			return resolved{v: map[string]any{"login": issue.UserLogin}, t: "User"}
		case "labels":
			var nodes []any
			for _, name := range issue.Labels {
				rec := repo.Labels[name]
				color := "ededed"
				desc := ""
				if rec != nil {
					color = rec.Color
					desc = rec.Description
				}
				nodes = append(nodes, map[string]any{"name": name, "color": color, "description": desc})
			}
			conn := &gqlConnection{
				isConnection: true, nodes: nodes, totalCount: len(nodes),
				pageInfo: map[string]any{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
				nodeType: "Label",
			}
			return resolved{v: conn, t: "LabelConnection", nt: "Label"}
		case "comments":
			list := commentsFor(repo, issue.Number)
			var nodes []any
			for _, c := range list {
				nodes = append(nodes, c)
			}
			conn := &gqlConnection{
				isConnection: true, nodes: nodes, totalCount: len(nodes),
				pageInfo: map[string]any{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
				nodeType: "IssueComment",
			}
			return resolved{v: conn, t: "IssueCommentConnection", nt: "IssueComment"}
		}
	case "PullRequest":
		repo, pull, ok := unwrapPull(src)
		if !ok {
			return resolved{v: nil}
		}
		switch field {
		case "id":
			return resolved{v: pullGlobalId(repo, pull)}
		case "number":
			return resolved{v: pull.Number}
		case "title":
			return resolved{v: pull.Title}
		case "body":
			return resolved{v: pull.Body}
		case "state":
			return resolved{v: pullState(pull)}
		case "merged":
			return resolved{v: pull.Merged}
		case "mergeable":
			if pull.Mergeable {
				return resolved{v: "MERGEABLE"}
			}
			return resolved{v: "CONFLICTING"}
		case "headRefName":
			return resolved{v: pull.HeadRef}
		case "baseRefName":
			return resolved{v: pull.BaseRef}
		case "url":
			return resolved{v: repoURL(repo) + "/pull/" + strconv.Itoa(pull.Number)}
		case "createdAt":
			return resolved{v: pull.CreatedAt}
		case "updatedAt":
			return resolved{v: pull.UpdatedAt}
		case "author":
			return resolved{v: map[string]any{"login": pull.UserLogin}, t: "User"}
		case "headRef":
			return resolved{v: map[string]any{"name": pull.HeadRef, "__repo": repo}, t: "Ref"}
		case "baseRef":
			return resolved{v: map[string]any{"name": pull.BaseRef, "__repo": repo}, t: "Ref"}
		case "files":
			paths := pullChangedFiles(repo, pull)
			first := len(paths)
			if n, ok := toInt(args["first"]); ok {
				first = n
			}
			if first > len(paths) {
				first = len(paths)
			}
			var nodes []any
			for _, p := range paths[:first] {
				nodes = append(nodes, map[string]any{"path": p, "additions": 0, "deletions": 0, "changeType": "ADDED"})
			}
			conn := &gqlConnection{
				isConnection: true, nodes: nodes, totalCount: len(paths),
				pageInfo: map[string]any{"hasNextPage": len(paths) > len(nodes), "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
				nodeType: "PullRequestChangedFile",
			}
			return resolved{v: conn, t: "PullRequestChangedFileConnection", nt: "PullRequestChangedFile"}
		}
	case "User":
		login := gqlStr(s["login"])
		switch field {
		case "login":
			return resolved{v: login}
		case "id":
			return resolved{v: toGlobalId("User", login)}
		case "name":
			return resolved{v: login}
		case "url":
			return resolved{v: "https://github.com/" + login}
		case "avatarUrl":
			return resolved{v: "https://avatars.githubusercontent.com/" + login}
		}
	case "Ref":
		name := gqlStr(s["name"])
		repo, _ := s["__repo"].(*RepoRecord)
		switch field {
		case "name":
			return resolved{v: name}
		case "id":
			if repo != nil {
				return resolved{v: toGlobalId("Ref", repo.Owner+"/"+repo.Name+"/refs/heads/"+name)}
			}
			return resolved{v: toGlobalId("Ref", name)}
		case "prefix":
			return resolved{v: "refs/heads/"}
		}
	case "Label":
		switch field {
		case "id":
			return resolved{v: toGlobalId("Label", gqlStr(s["name"]))}
		case "name":
			return resolved{v: s["name"]}
		case "color":
			if s["color"] == nil {
				return resolved{v: "ededed"}
			}
			return resolved{v: s["color"]}
		case "description":
			if s["description"] == nil {
				return resolved{v: ""}
			}
			return resolved{v: s["description"]}
		case "url":
			return resolved{v: ""}
		}
	case "PullRequestChangedFile":
		switch field {
		case "path":
			return resolved{v: s["path"]}
		case "additions":
			return resolved{v: 0}
		case "deletions":
			return resolved{v: 0}
		case "changeType":
			return resolved{v: "ADDED"}
		}
	case "IssueComment":
		// src is a *gqlCommentEntry here, not a map.
		if entry, ok := src.(*gqlCommentEntry); ok {
			switch field {
			case "id":
				return resolved{v: entry.ID}
			case "body":
				return resolved{v: entry.Body}
			case "createdAt":
				return resolved{v: entry.CreatedAt}
			case "updatedAt":
				return resolved{v: entry.CreatedAt}
			case "author":
				return resolved{v: map[string]any{"login": actor}, t: "User"}
			}
			return resolved{v: nil}
		}
		switch field {
		case "id":
			return resolved{v: s["id"]}
		case "body":
			return resolved{v: s["body"]}
		case "createdAt":
			if s["created_at"] != nil {
				return resolved{v: s["created_at"]}
			}
			return resolved{v: nowISO()}
		case "updatedAt":
			if s["created_at"] != nil {
				return resolved{v: s["created_at"]}
			}
			return resolved{v: nowISO()}
		case "author":
			return resolved{v: map[string]any{"login": actor}, t: "User"}
		}
	case "PageInfo":
		switch field {
		case "hasNextPage":
			return resolved{v: s["hasNextPage"] == true}
		case "hasPreviousPage":
			return resolved{v: s["hasPreviousPage"] == true}
		case "startCursor":
			return resolved{v: s["startCursor"]}
		case "endCursor":
			return resolved{v: s["endCursor"]}
		}
	case "__Type":
		switch field {
		case "kind":
			return resolved{v: s["kind"]}
		case "name":
			return resolved{v: s["name"]}
		case "description":
			return resolved{v: s["description"]}
		case "fields":
			return resolved{v: s["fields"], t: "__Field"}
		case "enumValues":
			return resolved{v: s["enumValues"], t: "__EnumValue"}
		case "inputFields":
			return resolved{v: s["inputFields"]}
		case "interfaces":
			return resolved{v: []any{}}
		case "possibleTypes":
			return resolved{v: s["possibleTypes"]}
		case "specifiedByURL":
			return resolved{v: nil}
		}
	case "__Field":
		switch field {
		case "name":
			return resolved{v: s["name"]}
		case "description":
			return resolved{v: s["description"]}
		case "args":
			return resolved{v: []any{}}
		case "type":
			if s["type"] != nil {
				return resolved{v: s["type"], t: "__Type"}
			}
			return resolved{v: map[string]any{"kind": "SCALAR", "name": "String", "ofType": nil}, t: "__Type"}
		case "isDeprecated":
			return resolved{v: false}
		case "deprecationReason":
			return resolved{v: nil}
		}
	case "__EnumValue":
		switch field {
		case "name":
			return resolved{v: s["name"]}
		case "description":
			return resolved{v: s["description"]}
		case "isDeprecated":
			return resolved{v: false}
		case "deprecationReason":
			return resolved{v: nil}
		}
	case "__Schema":
		switch field {
		case "description":
			return resolved{v: nil}
		case "types":
			var types []any
			for name := range typeKinds {
				if t := introspectionType(name); t != nil {
					types = append(types, t)
				}
			}
			return resolved{v: types}
		case "queryType":
			return resolved{v: map[string]any{"name": "Query", "kind": "OBJECT"}, t: "__Type"}
		case "mutationType":
			return resolved{v: map[string]any{"name": "Mutation", "kind": "OBJECT"}, t: "__Type"}
		case "subscriptionType":
			return resolved{v: nil}
		case "directives":
			return resolved{v: []any{}}
		}
	}

	if strings.HasSuffix(typeName, "Payload") {
		v, ok := s[field]
		if !ok || v == nil {
			return resolved{v: nil}
		}
		if typeName == "AddCommentPayload" && field == "commentEdge" {
			return resolved{v: v, t: "IssueCommentEdge"}
		}
		if field == "issue" {
			return resolved{v: v, t: "Issue"}
		}
		if field == "pullRequest" {
			return resolved{v: v, t: "PullRequest"}
		}
		if field == "clientMutationId" {
			return resolved{v: nil}
		}
		return resolved{v: v}
	}
	if strings.HasSuffix(typeName, "Edge") {
		if field == "node" {
			nodeType := typeName[:len(typeName)-len("Edge")]
			var node any
			if s != nil {
				node = s["node"]
			}
			// commentEdge's node may be a *gqlCommentEntry stored under "node"
			return resolved{v: node, t: nodeType}
		}
		if field == "cursor" {
			if s != nil {
				return resolved{v: s["cursor"]}
			}
			return resolved{v: nil}
		}
		return resolved{v: nil}
	}
	if strings.HasSuffix(typeName, "Connection") {
		conn, _ := src.(*gqlConnection)
		if conn == nil {
			return resolved{v: nil}
		}
		switch field {
		case "nodes":
			return resolved{v: conn.nodes, t: conn.nodeType}
		case "edges":
			edges := make([]any, 0, len(conn.nodes))
			for i, n := range conn.nodes {
				edges = append(edges, map[string]any{"node": n, "cursor": cursorFor(i + 1)})
			}
			return resolved{v: edges, t: conn.nodeType + "Edge"}
		case "totalCount":
			return resolved{v: conn.totalCount}
		case "pageInfo":
			return resolved{v: conn.pageInfo, t: "PageInfo"}
		}
	}
	return resolved{v: nil}
}

func executeSelection(typeName string, src any, selections []*selection, ctx *execCtx) map[string]any {
	out := make(map[string]any)
	known := make(map[string]bool)
	for _, f := range knownFields[typeName] {
		known[f] = true
	}
	for _, sel := range selections {
		if sel.kind == "spread" {
			if frag, ok := ctx.fragments[sel.name]; ok {
				for k, v := range executeSelection(typeName, src, frag, ctx) {
					out[k] = v
				}
			}
			continue
		}
		if sel.kind == "fragment" {
			if sel.typeCondition == "" || sel.typeCondition == typeName {
				for k, v := range executeSelection(typeName, src, sel.selections, ctx) {
					out[k] = v
				}
			} else if m, ok := src.(map[string]any); ok && fmt.Sprint(m["__typename"]) == sel.typeCondition {
				inner := m["__src"]
				for k, v := range executeSelection(sel.typeCondition, inner, sel.selections, ctx) {
					out[k] = v
				}
			}
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		if sel.name == "__typename" {
			out[key] = typeName
			continue
		}
		if !known[sel.name] {
			ctx.errors = append(ctx.errors, map[string]any{"message": "Cannot query field \"" + sel.name + "\" on type \"" + typeName + "\"."})
			out[key] = nil
			continue
		}
		args := resolveArgs(sel, ctx)
		res := fieldValue(typeName, src, sel.name, args, ctx)
		if len(sel.selections) == 0 || res.v == nil {
			out[key] = res.v
			continue
		}
		// Recurse into sub-selections.
		if conn, ok := res.v.(*gqlConnection); ok && conn.isConnection {
			nodeType := res.nt
			if nodeType == "" {
				nodeType = conn.nodeType
			}
			if sel.name == "nodes" {
				var arr []any
				for _, n := range conn.nodes {
					arr = append(arr, executeSelection(nodeType, n, sel.selections, ctx))
				}
				out[key] = arr
			} else if sel.name == "edges" {
				var arr []any
				for i, n := range conn.nodes {
					arr = append(arr, executeSelection(nodeType+"Edge", map[string]any{"node": n, "cursor": cursorFor(i + 1)}, sel.selections, ctx))
				}
				out[key] = arr
			} else {
				out[key] = executeSelection(res.t, res.v, sel.selections, ctx)
			}
			continue
		}
		if arr, ok := res.v.([]any); ok {
			itemType := res.t
			var mapped []any
			for _, item := range arr {
				if item != nil {
					if _, isMap := item.(map[string]any); isMap && len(sel.selections) > 0 && len(knownFields[itemType]) > 0 {
						mapped = append(mapped, executeSelection(itemType, item, sel.selections, ctx))
						continue
					}
					if _, isEntry := item.(*gqlCommentEntry); isEntry && len(sel.selections) > 0 && len(knownFields[itemType]) > 0 {
						mapped = append(mapped, executeSelection(itemType, item, sel.selections, ctx))
						continue
					}
				}
				mapped = append(mapped, item)
			}
			if mapped == nil {
				mapped = []any{}
			}
			out[key] = mapped
			continue
		}
		// Node wrapper (node(id:)): __typename + __src
		if m, ok := res.v.(map[string]any); ok {
			if typename, hasTypename := m["__typename"]; hasTypename {
				if srcVal, hasSrc := m["__src"]; hasSrc {
					out[key] = executeSelection(fmt.Sprint(typename), srcVal, sel.selections, ctx)
					continue
				}
			}
		}
		out[key] = executeSelection(res.t, res.v, sel.selections, ctx)
	}
	return out
}

func executeOperation(op *operation, vars map[string]any, store *Store, fragments map[string][]*selection) map[string]any {
	ctx := &execCtx{store: store, vars: vars, fragments: fragments}
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	data := executeSelection(rootType, map[string]any{}, op.selections, ctx)
	if len(ctx.errors) > 0 {
		return map[string]any{"data": data, "errors": ctx.errors}
	}
	return map[string]any{"data": data}
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func gqlSendJSON(status int, body any, requestID string) Response {
	b, err := marshalJSON(body)
	if err != nil {
		b = []byte(`{"message":"Internal Server Error"}`)
	}
	return Response{Status: status, Headers: [][2]string{
		{"content-type", "application/json"},
		{"x-github-request-id", requestID},
	}, Body: b}
}

func handleGraphql(req *Request, store *Store) (Response, bool) {
	if req.Path != "/api/graphql" {
		return Response{}, false
	}
	requestID := nextRequestID()
	if req.Method != "POST" {
		return gqlSendJSON(405, map[string]any{"message": "method not allowed"}, requestID), true
	}
	if !gqlAuthed(req) {
		return gqlSendJSON(401, map[string]any{"message": "unauthorized"}, requestID), true
	}
	var body any
	if err := jsonUnmarshal(req.Body, &body); err != nil {
		return gqlSendJSON(400, map[string]any{"message": "malformed JSON body"}, requestID), true
	}
	isArray := false
	var ops []map[string]any
	if arr, ok := body.([]any); ok {
		isArray = true
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				ops = append(ops, m)
			} else {
				ops = append(ops, map[string]any{})
			}
		}
	} else if m, ok := body.(map[string]any); ok {
		ops = []map[string]any{m}
	} else {
		return gqlSendJSON(400, map[string]any{"message": "malformed JSON body"}, requestID), true
	}
	var results []map[string]any
	for _, entry := range ops {
		query, _ := entry["query"].(string)
		if strings.TrimSpace(query) == "" {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "query is required"}}})
			continue
		}
		variables, _ := entry["variables"].(map[string]any)
		if variables == nil {
			variables = map[string]any{}
		}
		parser := &gqlParser{tokens: tokenize(query)}
		operations, fragments, err := parser.parseDocument()
		if err != nil {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "syntax error: " + err.Error()}}})
			continue
		}
		if len(operations) == 0 {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "no operation found"}}})
			continue
		}
		op := operations[0]
		if operationName, ok := entry["operationName"].(string); ok {
			for _, o := range operations {
				if o.name == operationName {
					op = o
					break
				}
			}
		}
		vars := make(map[string]any)
		for k, v := range variables {
			vars[k] = v
		}
		for k, v := range op.varDefaults {
			if _, ok := vars[k]; !ok {
				dummyCtx := &execCtx{store: store, vars: vars, fragments: fragments}
				vars[k] = resolveValue(v, dummyCtx)
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": fmt.Sprint(r)}}})
				}
			}()
			results = append(results, executeOperation(op, vars, store, fragments))
		}()
	}
	if isArray {
		out := make([]any, 0, len(results))
		for _, r := range results {
			out = append(out, r)
		}
		return gqlSendJSON(200, out, requestID), true
	}
	return gqlSendJSON(200, results[0], requestID), true
}
