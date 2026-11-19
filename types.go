// Shared data model for the github-core-clone-01 Go forge.
// Port of the Node entry's types.ts: the contract between modules.
// Keep these shapes identical to the Node build so every surface
// (git, REST, GraphQL, MCP) renders the same bytes.

package main

import (
	"encoding/base64"
	"strings"
	"time"
)

// GitObject is one git object: raw content WITHOUT the "<type> <len>\0" header.
type GitObject struct {
	Type string // "blob" | "tree" | "commit" | "tag"
	Data []byte
}

type LabelRecord struct {
	Name        string
	Color       string
	Description string
}

type IssueRecord struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	UserLogin string
	CreatedAt string
	UpdatedAt string
}

type PullRecord struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	Mergeable      bool
	HeadRef        string // branch name, e.g. "feature"
	BaseRef        string // branch name, e.g. "main"
	HeadSha        string // tip commit of head branch at creation (updated on push)
	BaseSha        string // tip commit of base branch at creation
	MergeCommitSha string // "" when none (Node: null)
	UserLogin      string
	CreatedAt      string
	UpdatedAt      string
}

type RepoRecord struct {
	Owner         string
	Name          string
	Description   string
	DefaultBranch string // "main"
	CreatedAt     string
	UpdatedAt     string
	Refs          map[string]string     // full refname "refs/heads/main" -> commit sha
	Objects       map[string]*GitObject // sha -> object
	Labels        map[string]*LabelRecord
	Issues        map[int]*IssueRecord
	Pulls         map[int]*PullRecord
	IssueSeq      int
	PullSeq       int
	// Extra fields the Node build carries on the record as (repo as any).id / .private.
	ID      int
	Private bool

	// Per-repo caches (Node keeps these in WeakMaps keyed by repo).
	metaCache map[string]commitMeta
	reachable map[string]bool
	// Fetch-response cache, keyed like the Node build (bounded, clears when full).
	fetchCache map[string][]byte
	// Fingerprinted v0 advertisement.
	v0AdFp  string
	v0AdBuf []byte
	// Serialized GET-issue responses, per issue number.
	issueCache map[int]*issueCacheEntry
	// GraphQL comment store (Node keeps it in a WeakMap keyed by repo).
	gqlComments        map[int][]*gqlCommentEntry
	gqlCommentVersions map[int]int
}

func newRepoRecord(owner, name, description string) *RepoRecord {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return &RepoRecord{
		Owner:         owner,
		Name:          name,
		Description:   description,
		DefaultBranch: "main",
		CreatedAt:     now,
		UpdatedAt:     now,
		Refs:          make(map[string]string),
		Objects:       make(map[string]*GitObject),
		Labels:        make(map[string]*LabelRecord),
		Issues:        make(map[int]*IssueRecord),
		Pulls:         make(map[int]*PullRecord),
	}
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// toGlobalId: GraphQL global ids are base64 of "Type:id".
func toGlobalId(typ, id string) string {
	return base64.StdEncoding.EncodeToString([]byte(typ + ":" + id))
}

func fromGlobalId(gid string) (typ, id string, ok bool) {
	b, err := base64.StdEncoding.DecodeString(gid)
	if err != nil {
		return "", "", false
	}
	i := strings.IndexByte(string(b), ':')
	if i < 0 {
		return "", "", false
	}
	return string(b[:i]), string(b[i+1:]), true
}
