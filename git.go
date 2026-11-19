// Git core for the github-core-clone-01 Go forge: object model,
// tree/commit helpers, ancestry, and the git smart-HTTP surface
// (upload-pack v2 + v0, receive-pack v0). Go stdlib only, no git
// binary, no network. Port of the Node entry's src/git.ts.
//
// The wire behaviour mirrors what the suite's client sends and parses:
//   - info/refs answers v2 when Git-Protocol: version=2 is present on
//     git-upload-pack, v0 otherwise; receive-pack is always v0.
//   - v0 advertisements open with "# service=..." + flush; v2 opens
//     with "version 2". Empty repos advertise the zero-id capabilities^{}
//     line.
//   - Packs are plain objects only (no deltas), zlib-deflated, with a
//     SHA-1 trailer; fetch responses are side-band-64k framed.
//   - receive-pack applies only fast-forward updates (or creates/deletes)
//     and reports per-ref ok/ng lines under report-status.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
const agentName = "makefun-git/1.0"

// Largest side-band payload chunk: a pkt-line holds 65520 bytes incl. header.
const sidebandChunk = 32768

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// object model
// ---------------------------------------------------------------------------

var sha1Pool = sync.Pool{New: func() any { return sha1.New() }}

func sha1Object(typ string, data []byte) string {
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	sum := h.Sum(nil)
	sha1Pool.Put(h)
	return hex.EncodeToString(sum)
}

func storeObject(repo *RepoRecord, typ string, data []byte) string {
	sha := sha1Object(typ, data)
	repo.Objects[sha] = &GitObject{Type: typ, Data: data}
	return sha
}

func getObject(repo *RepoRecord, sha string) *GitObject {
	return repo.Objects[sha]
}

type TreeEntry struct {
	Mode string
	Name string
	Sha  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			return nil, errors.New("tree entry is truncated")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry is truncated")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			Sha:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

func isDirMode(mode string) bool { return mode == "40000" || mode == "040000" }

// git's tree sort: byte order of the name, a directory compared as name + '/'.
func compareTreeEntries(a, b TreeEntry) int {
	an := a.Name
	if isDirMode(a.Mode) {
		an += "/"
	}
	bn := b.Name
	if isDirMode(b.Mode) {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

func buildTree(repo *RepoRecord, entries []TreeEntry) string {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeEntries(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.Sha)
		buf.Write(raw)
	}
	return storeObject(repo, "tree", buf.Bytes())
}

type parsedCommit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) parsedCommit {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var message string
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	var out parsedCommit
	out.Message = message
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.IndexByte(line, ' ')
		if space < 0 {
			continue
		}
		key, value := line[:space], line[space+1:]
		switch key {
		case "tree":
			out.Tree = value
		case "parent":
			out.Parents = append(out.Parents, value)
		case "author":
			out.Author = value
		case "committer":
			out.Committer = value
		}
	}
	return out
}

func createCommit(repo *RepoRecord, treeSha string, parents []string, message string, authorName, authorEmail string) string {
	timestamp := currentUnixSec()
	who := fmt.Sprintf("%s <%s> %d +0000", authorName, authorEmail, timestamp)
	var lines []string
	lines = append(lines, "tree "+treeSha)
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+who, "committer "+who, "")
	body := message
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return storeObject(repo, "commit", []byte(strings.Join(lines, "\n")+"\n"+body))
}

// Recursive worker: returns the new tree sha, or "" when the subtree
// became empty (so the caller drops the directory entry, as git does).
func updateInTree(repo *RepoRecord, baseTreeSha string, hasBase bool, parts []string, blobSha string, hasBlob bool, mode string) string {
	var entries []TreeEntry
	if hasBase {
		if base := getObject(repo, baseTreeSha); base != nil && base.Type == "tree" {
			entries, _ = parseTree(base.Data)
		}
	}
	name := parts[0]
	idx := -1
	for i, e := range entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if len(parts) == 1 {
		if !hasBlob {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			entryMode := mode
			if entryMode == "" {
				if idx >= 0 {
					entryMode = entries[idx].Mode
				} else {
					entryMode = "100644"
				}
			}
			entry := TreeEntry{Mode: entryMode, Name: name, Sha: blobSha}
			if idx >= 0 {
				entries[idx] = entry
			} else {
				entries = append(entries, entry)
			}
		}
	} else {
		var childSha string
		var hasChild bool
		if idx >= 0 && isDirMode(entries[idx].Mode) {
			childSha, hasChild = entries[idx].Sha, true
		}
		next := updateInTree(repo, childSha, hasChild, parts[1:], blobSha, hasBlob, mode)
		if next == "" {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			entry := TreeEntry{Mode: "40000", Name: name, Sha: next}
			if idx >= 0 {
				entries[idx] = entry
			} else {
				entries = append(entries, entry)
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return buildTree(repo, entries)
}

func updatePathInTree(repo *RepoRecord, baseTreeSha string, hasBase bool, parts []string, blobSha string, hasBlob bool, mode string) string {
	if len(parts) == 0 {
		if hasBase {
			return baseTreeSha
		}
		return buildTree(repo, nil)
	}
	next := updateInTree(repo, baseTreeSha, hasBase, parts, blobSha, hasBlob, mode)
	if next != "" {
		return next
	}
	return buildTree(repo, nil)
}

func getPathEntry(repo *RepoRecord, commitSha, path string) (sha string, data []byte, mode, typ string, ok bool) {
	commitObj := getObject(repo, commitSha)
	if commitObj == nil || commitObj.Type != "commit" {
		return "", nil, "", "", false
	}
	tree := parseCommit(commitObj.Data).Tree
	segments := []string{}
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	curSha := tree
	curMode := "40000"
	curObj := getObject(repo, curSha)
	if len(segments) == 0 {
		if curObj == nil {
			return "", nil, "", "", false
		}
		return curSha, curObj.Data, "40000", "tree", true
	}
	for i, seg := range segments {
		if curObj == nil || curObj.Type != "tree" {
			return "", nil, "", "", false
		}
		entries, err := parseTree(curObj.Data)
		if err != nil {
			return "", nil, "", "", false
		}
		var entry *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				entry = &entries[j]
				break
			}
		}
		if entry == nil {
			return "", nil, "", "", false
		}
		curSha = entry.Sha
		curMode = entry.Mode
		curObj = getObject(repo, curSha)
		if i == len(segments)-1 {
			if curObj == nil {
				// A gitlink names a commit this repo does not hold.
				if curMode == "160000" {
					return curSha, []byte{}, curMode, "commit", true
				}
				return "", nil, "", "", false
			}
			return curSha, curObj.Data, curMode, curObj.Type, true
		}
	}
	return "", nil, "", "", false
}

// ---------------------------------------------------------------------------
// ancestry
// ---------------------------------------------------------------------------

// Parsed commit header (tree + parents), cached per repo: the push path
// walks ancestry on every update, and re-parsing the same commit buffers
// each time made it quadratic in history length.
type commitMeta struct {
	tree    string
	parents []string
}

func (r *RepoRecord) commitMeta(sha string) (commitMeta, bool) {
	if r.metaCache == nil {
		r.metaCache = make(map[string]commitMeta)
	}
	if m, ok := r.metaCache[sha]; ok {
		return m, true
	}
	obj := getObject(r, sha)
	if obj == nil || obj.Type != "commit" {
		return commitMeta{}, false
	}
	parsed := parseCommit(obj.Data)
	m := commitMeta{tree: parsed.Tree, parents: parsed.Parents}
	r.metaCache[sha] = m
	return m, true
}

func commitParents(repo *RepoRecord, sha string) []string {
	m, ok := repo.commitMeta(sha)
	if !ok {
		return nil
	}
	return m.parents
}

// Commits known reachable from a ref, per repo. Objects are never removed
// from a repo, and a commit only enters this set once its ancestry has been
// verified present (receive-pack) or built complete (REST), so stopping a
// completeness walk here is sound.
func (r *RepoRecord) reachableSet() map[string]bool {
	if r.reachable == nil {
		r.reachable = make(map[string]bool)
	}
	return r.reachable
}

// noteReachable records that oid (and its ancestors, up to already-known
// ones) is reachable from a ref.
func noteReachable(repo *RepoRecord, oid string) {
	set := repo.reachableSet()
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if set[cur] {
			continue
		}
		obj := getObject(repo, cur)
		if obj == nil {
			continue
		}
		if obj.Type == "commit" {
			set[cur] = true
			for _, p := range commitParents(repo, cur) {
				stack = append(stack, p)
			}
		} else if obj.Type == "tag" {
			stack = append(stack, peelTag(repo, cur))
		}
	}
}

// isComplete: is every commit reachable from oid present in the repo
// (tags peeled)? Walks cached parent lists and stops at commits already
// known reachable, so a push stays O(new history).
func isComplete(repo *RepoRecord, oid string) bool {
	known := repo.reachableSet()
	seen := make(map[string]bool)
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] || known[cur] {
			continue
		}
		seen[cur] = true
		obj := getObject(repo, cur)
		if obj == nil {
			return false
		}
		if obj.Type == "commit" {
			parents := commitParents(repo, cur)
			if parents == nil {
				// commitMeta returns nil parents only when the object is
				// not a commit; a commit with no parents has an empty
				// (non-nil) slice. Distinguish via the lookup.
				if _, ok := repo.commitMeta(cur); !ok {
					return false
				}
			}
			for _, p := range parents {
				stack = append(stack, p)
			}
		} else if obj.Type == "tag" {
			stack = append(stack, peelTag(repo, cur))
		}
	}
	return true
}

func findMergeBase(repo *RepoRecord, a, b string) string {
	if a == b {
		if obj := getObject(repo, a); obj != nil && obj.Type == "commit" {
			return a
		}
		return ""
	}
	ancestorsA := make(map[string]bool)
	stackA := []string{a}
	for len(stackA) > 0 {
		cur := stackA[len(stackA)-1]
		stackA = stackA[:len(stackA)-1]
		if ancestorsA[cur] {
			continue
		}
		parents := commitParents(repo, cur)
		if parents == nil {
			if _, ok := repo.commitMeta(cur); !ok {
				continue
			}
		}
		ancestorsA[cur] = true
		for _, p := range parents {
			stackA = append(stackA, p)
		}
	}
	// BFS from b: the first common commit is the merge base closest to b.
	seenB := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seenB[cur] {
			continue
		}
		seenB[cur] = true
		if ancestorsA[cur] {
			return cur
		}
		parents := commitParents(repo, cur)
		if parents == nil {
			if _, ok := repo.commitMeta(cur); !ok {
				continue
			}
		}
		for _, p := range parents {
			queue = append(queue, p)
		}
	}
	return ""
}

func isAncestor(repo *RepoRecord, anc, desc string) bool {
	if anc == desc {
		obj := getObject(repo, desc)
		return obj != nil && obj.Type == "commit"
	}
	seen := make(map[string]bool)
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		parents := commitParents(repo, cur)
		if parents == nil {
			if _, ok := repo.commitMeta(cur); !ok {
				continue
			}
		}
		for _, p := range parents {
			stack = append(stack, p)
		}
	}
	return false
}

func listCommits(repo *RepoRecord, tipSha string, limit int) []string {
	var out []string
	seen := make(map[string]bool)
	cur := tipSha
	for cur != "" {
		if seen[cur] {
			break
		}
		seen[cur] = true
		parents := commitParents(repo, cur)
		if parents == nil {
			if _, ok := repo.commitMeta(cur); !ok {
				break
			}
		}
		out = append(out, cur)
		if limit > 0 && len(out) >= limit {
			break
		}
		if len(parents) == 0 {
			break
		}
		cur = parents[0]
	}
	return out
}

// ---------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	hexLen := fmt.Sprintf("%04x", len(payload)+4)
	copy(out, hexLen)
	copy(out[4:], payload)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type decodedPacket struct {
	kind    string // "data" | "flush" | "delim" | "response-end"
	payload []byte
}

// decodePktLines decodes pkt-lines from the start of buf. Returns the
// packets and the offset just past the last complete packet. With
// stopAtFlush, decoding stops right after the first flush packet.
func decodePktLines(buf []byte, stopAtFlush bool) ([]decodedPacket, int, error) {
	var packets []decodedPacket
	offset := 0
	for offset+4 <= len(buf) {
		head := string(buf[offset : offset+4])
		length, err := strconv.ParseInt(head, 16, 32)
		if err != nil || length < 0 {
			return nil, 0, fmt.Errorf("invalid pkt-line length %q", head)
		}
		if length == 0 {
			packets = append(packets, decodedPacket{kind: "flush"})
			offset += 4
			if stopAtFlush {
				return packets, offset, nil
			}
			continue
		}
		if length == 1 {
			packets = append(packets, decodedPacket{kind: "delim"})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, decodedPacket{kind: "response-end"})
			offset += 4
			continue
		}
		if length == 3 || length > 65520 {
			return nil, 0, fmt.Errorf("invalid pkt-line length %d", length)
		}
		if offset+int(length) > len(buf) {
			return nil, 0, errors.New("truncated pkt-line")
		}
		packets = append(packets, decodedPacket{kind: "data", payload: buf[offset+4 : offset+int(length)]})
		offset += int(length)
	}
	if offset != len(buf) {
		return nil, 0, errors.New("truncated pkt-line header")
	}
	return packets, offset, nil
}

// packetLines: data-packet payloads as strings, control packets dropped.
func packetLines(packets []decodedPacket) []string {
	var lines []string
	for _, p := range packets {
		if p.kind == "data" {
			lines = append(lines, string(p.payload))
		}
	}
	return lines
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

// peelTag peels an annotated tag (recursively) to its target object id.
func peelTag(repo *RepoRecord, oid string) string {
	cur := oid
	for {
		obj := getObject(repo, cur)
		if obj == nil || obj.Type != "tag" {
			return cur
		}
		blank := bytes.Index(obj.Data, []byte("\n\n"))
		headerEnd := len(obj.Data)
		if blank >= 0 {
			headerEnd = blank
		}
		target := ""
		for _, line := range strings.Split(string(obj.Data[:headerEnd]), "\n") {
			if strings.HasPrefix(line, "object ") {
				target = line[7:]
			}
		}
		if !isHex40(target) {
			return cur
		}
		cur = target
	}
}

// addTreeClosure adds a tree and everything under it (skipping gitlinks) to out.
func addTreeClosure(repo *RepoRecord, treeSha string, out map[string]bool) {
	stack := []string{treeSha}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		obj := getObject(repo, cur)
		if obj == nil || obj.Type != "tree" {
			continue
		}
		out[cur] = true
		entries, err := parseTree(obj.Data)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Mode == "160000" {
				continue
			}
			if isDirMode(e.Mode) {
				stack = append(stack, e.Sha)
			} else {
				out[e.Sha] = true
			}
		}
	}
}

// collectReachable: every object reachable from roots (commit -> tree +
// parents, tag -> target, tree -> entries). Unknown roots are skipped
// when allowMissing.
func collectReachable(repo *RepoRecord, roots []string, allowMissing bool) (map[string]bool, error) {
	seen := make(map[string]bool)
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := getObject(repo, oid)
		if obj == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("object %s is missing", oid)
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			commit := parseCommit(obj.Data)
			addTreeClosure(repo, commit.Tree, seen)
			for _, p := range commit.Parents {
				stack = append(stack, p)
			}
		case "tag":
			stack = append(stack, peelTag(repo, oid))
		case "tree":
			addTreeClosure(repo, oid, seen)
		}
	}
	return seen, nil
}

// collectReachableCutoff: reachable from roots, but never following the
// parents of a commit in cutoff (the client's shallow boundaries).
func collectReachableCutoff(repo *RepoRecord, roots []string, cutoff map[string]bool) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := getObject(repo, oid)
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			commit := parseCommit(obj.Data)
			addTreeClosure(repo, commit.Tree, seen)
			if !cutoff[oid] {
				for _, p := range commit.Parents {
					stack = append(stack, p)
				}
			}
		case "tag":
			stack = append(stack, peelTag(repo, oid))
		case "tree":
			addTreeClosure(repo, oid, seen)
		}
	}
	return seen
}

type depthWalk struct {
	objects  map[string]bool
	commits  map[string]int // included commits and their distance from a want (want = 1)
	boundary []string       // commits at exactly maxDepth that still have parents
}

// collectDepthLimited: depth-limited walk from roots: a commit at distance
// d is kept iff d <= maxDepth.
func collectDepthLimited(repo *RepoRecord, roots []string, maxDepth int) depthWalk {
	objects := make(map[string]bool)
	commits := make(map[string]int)
	var boundary []string
	type item struct {
		oid   string
		depth int
	}
	var queue []item
	for _, root := range roots {
		obj := getObject(repo, root)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case "tag":
			objects[root] = true
			target := peelTag(repo, root)
			targetObj := getObject(repo, target)
			if targetObj != nil && targetObj.Type == "commit" {
				queue = append(queue, item{target, 1})
			} else if targetObj != nil && targetObj.Type == "tree" {
				addTreeClosure(repo, target, objects)
			} else if targetObj != nil {
				objects[target] = true
			}
		case "commit":
			queue = append(queue, item{root, 1})
		case "tree":
			addTreeClosure(repo, root, objects)
		default:
			objects[root] = true
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := commits[cur.oid]; ok {
			continue
		}
		obj := getObject(repo, cur.oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		commits[cur.oid] = cur.depth
		objects[cur.oid] = true
		commit := parseCommit(obj.Data)
		addTreeClosure(repo, commit.Tree, objects)
		if cur.depth < maxDepth {
			for _, p := range commit.Parents {
				queue = append(queue, item{p, cur.depth + 1})
			}
		} else if len(commit.Parents) > 0 {
			boundary = append(boundary, cur.oid)
		}
	}
	return depthWalk{objects: objects, commits: commits, boundary: boundary}
}

type fetchPlan struct {
	send      map[string]bool
	shallow   []string
	unshallow []string
}

func planFetch(repo *RepoRecord, wants, haves, clientShallow []string, depth int, hasDepth bool) fetchPlan {
	var wantedObjects map[string]bool
	var wantedCommits map[string]int
	var boundary []string
	if hasDepth {
		walk := collectDepthLimited(repo, wants, depth)
		wantedObjects = walk.objects
		wantedCommits = walk.commits
		boundary = walk.boundary
	} else {
		wantedObjects, _ = collectReachable(repo, wants, true)
		wantedCommits = make(map[string]int)
		for oid := range wantedObjects {
			if obj := getObject(repo, oid); obj != nil && obj.Type == "commit" {
				wantedCommits[oid] = 0
			}
		}
	}
	cutoff := make(map[string]bool)
	for _, s := range clientShallow {
		cutoff[s] = true
	}
	held := collectReachableCutoff(repo, haves, cutoff)
	send := make(map[string]bool)
	for oid := range wantedObjects {
		if !held[oid] {
			send[oid] = true
		}
	}
	var unshallow []string
	for _, oid := range clientShallow {
		if containsStr(boundary, oid) {
			continue
		}
		if _, ok := wantedCommits[oid]; !ok {
			continue
		}
		parents := commitParents(repo, oid)
		if parents == nil {
			if _, ok := repo.commitMeta(oid); !ok {
				continue
			}
		}
		for _, p := range parents {
			if _, ok := wantedCommits[p]; ok {
				unshallow = append(unshallow, oid)
				break
			}
		}
	}
	return fetchPlan{send: send, shallow: boundary, unshallow: unshallow}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// packfile write / read (plain objects, no deltas on write; deltas resolved on read)
// ---------------------------------------------------------------------------

func packObjectHeader(typ string, size int) []byte {
	code := typeCode[typ]
	var bytesOut []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		bytesOut = append(bytesOut, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	bytesOut = append(bytesOut, byte(first))
	return bytesOut
}

var zlibBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func deflateBytes(data []byte) []byte {
	buf := zlibBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	w, _ := zlib.NewWriterLevel(buf, zlib.DefaultCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	zlibBufPool.Put(buf)
	return out
}

func buildPack(repo *RepoRecord, oids map[string]bool) []byte {
	list := make([]string, 0, len(oids))
	for oid := range oids {
		list = append(list, oid)
	}
	sort.Strings(list)
	var parts bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	header[8] = byte(len(list) >> 24)
	header[9] = byte(len(list) >> 16)
	header[10] = byte(len(list) >> 8)
	header[11] = byte(len(list))
	parts.Write(header)
	for _, oid := range list {
		obj := getObject(repo, oid)
		if obj == nil {
			continue
		}
		parts.Write(packObjectHeader(obj.Type, len(obj.Data)))
		parts.Write(deflateBytes(obj.Data))
	}
	body := parts.Bytes()
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	h.Write(body)
	sum := h.Sum(nil)
	sha1Pool.Put(h)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum...)
	return out
}

// sidebandPack frames a pack in side-band-64k packets (band 1), then a flush.
func sidebandPack(pack []byte) []byte {
	var parts bytes.Buffer
	for offset := 0; offset < len(pack); offset += sidebandChunk {
		end := offset + sidebandChunk
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[offset:end]
		payload := make([]byte, 1+len(chunk))
		payload[0] = 1
		copy(payload[1:], chunk)
		parts.Write(pktLine(payload))
	}
	parts.Write(pktFlush)
	return parts.Bytes()
}

func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return size, offset, first, nil
}

func readOfsBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[offset]
		offset++
		value = ((value + 1) * 128) + int(b&0x7f)
	}
	return value, offset, nil
}

// countReader wraps a bytes.Reader and counts the bytes the zlib layer
// actually pulls, so inflateAt can pin down where a stream ends: Go's
// zlib reader does not report how many input bytes it consumed.
type countReader struct {
	r *bytes.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func inflateOnce(buf []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// inflateAt inflates the zlib stream starting at offset in buf and
// returns the data plus the offset just past the stream. Go's zlib.Reader
// reads exactly the bytes of the stream (verified: zero over-read for
// sizes 0..50000 with trailing bytes present), so the counting reader
// gives the exact stream end in a single pass — no re-inflation scan.
func inflateAt(buf []byte, offset int, expectedSize int) ([]byte, int, error) {
	cr := &countReader{r: bytes.NewReader(buf[offset:])}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	data, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	if len(data) != expectedSize {
		return nil, 0, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", offset, expectedSize, len(data))
	}
	end := offset + int(cr.n)
	if end < offset || end > len(buf) {
		return nil, 0, fmt.Errorf("zlib stream at offset %d has inconsistent end %d", offset, end)
	}
	return data, end, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type rawPackEntry struct {
	offset     int
	typeCode   int
	size       int
	data       []byte
	baseOffset int
	hasBaseOff bool
	baseOid    string
	hasBaseOid bool
}

// readPack reads a packfile (v2/v3), resolving OFS_DELTA and REF_DELTA
// (thin-pack bases come from resolveBase, i.e. objects the repo already
// holds). Verifies the trailer checksum and every inflated size.
func readPack(pack []byte, resolveBase func(oid string) *GitObject) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	h.Write(pack[:trailerStart])
	computed := hex.EncodeToString(h.Sum(nil))
	sha1Pool.Put(h)
	if trailer != computed {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	var raw []*rawPackEntry
	byOffset := make(map[int]*rawPackEntry)
	offset := 12
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		tc := (int(first) >> 4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, nextOff, err := readOfsBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = nextOff
			if base <= 0 || base > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOffset, base)
			}
			entry.baseOffset = entryOffset - base
			entry.hasBaseOff = true
		} else if tc == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			entry.hasBaseOid = true
			offset += 20
		} else if codeType[tc] == "" {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, tc)
		}
		inflated, nextOff, err := inflateAt(pack[:trailerStart], offset, size)
		if err != nil {
			return nil, err
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	objects := make(map[string]*GitObject)
	resolvedByOid := make(map[string]*rawPackEntry)
	resolved := make(map[int]struct {
		typ  string
		data []byte
		oid  string
	})
	resolving := make(map[int]bool)
	var resolve func(entry *rawPackEntry) (string, []byte, string, error)
	resolve = func(entry *rawPackEntry) (string, []byte, string, error) {
		if cached, ok := resolved[entry.offset]; ok {
			return cached.typ, cached.data, cached.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ string
		var data []byte
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				return "", nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			}
			baseTyp, baseData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseTyp
			data, err = applyDelta(baseData, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			if baseEntry, ok := resolvedByOid[entry.baseOid]; ok {
				baseTyp, baseData, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = baseTyp
				data, err = applyDelta(baseData, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				external := resolveBase(entry.baseOid)
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor on the server", entry.baseOid)
				}
				typ = external.Type
				var err error
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := sha1Object(typ, data)
		resolved[entry.offset] = struct {
			typ  string
			data []byte
			oid  string
		}{typ, data, oid}
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}
	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: typ, Data: data}
		}
	}
	return objects, nil
}
