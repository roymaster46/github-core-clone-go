// Git smart-HTTP handlers for the Go forge: info/refs, upload-pack
// (v2 ls-refs/fetch, v0 fetch) and receive-pack v0.
// Port of the second half of the Node entry's src/git.ts.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func sendJSONResponse(status int, body any) Response {
	b, err := marshalJSON(body)
	if err != nil {
		b = []byte(`{"message":"Internal Server Error"}`)
		status = 500
	}
	return jsonResponse(status, b)
}

func sendBufferResponse(status int, contentType string, body []byte) Response {
	return textResponse(status, contentType, body)
}

// ---------------------------------------------------------------------------
// ref advertisement
// ---------------------------------------------------------------------------

type refLine struct {
	oid    string
	name   string
	peeled string
}

// listRefs: sorted refs (heads then tags by name sort), with peeled
// targets for annotated tags.
func listRefs(repo *RepoRecord) []refLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []refLine
	for _, name := range names {
		oid := repo.Refs[name]
		entry := refLine{oid: oid, name: name}
		obj := getObject(repo, oid)
		if strings.HasPrefix(name, "refs/tags/") && obj != nil && obj.Type == "tag" {
			target := peelTag(repo, oid)
			if target != oid {
				if t := getObject(repo, target); t != nil && t.Type != "tag" {
					entry.peeled = target
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

func v0Advertisement(repo *RepoRecord, service string) []byte {
	// Fingerprint cache: the ad is fully determined by the ref set.
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var fp strings.Builder
	fp.WriteString(service)
	for _, name := range names {
		fp.WriteString("\x00")
		fp.WriteString(name)
		fp.WriteString("=")
		fp.WriteString(repo.Refs[name])
	}
	fpStr := fp.String()
	if repo.v0AdBuf != nil && repo.v0AdFp == fpStr {
		return repo.v0AdBuf
	}
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k thin-pack ofs-delta shallow agent=" + agentName + " symref=HEAD:refs/heads/main"
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + agentName + " symref=HEAD:refs/heads/main"
	}
	var parts bytes.Buffer
	parts.Write(pktLineStr("# service=" + service + "\n"))
	parts.Write(pktFlush)
	refs := listRefs(repo)
	if len(refs) == 0 {
		parts.Write(pktLineStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		headOid := repo.Refs["refs/heads/main"]
		if headOid == "" {
			headOid = zeroOID
		}
		parts.Write(pktLineStr(headOid + " HEAD\x00" + caps + "\n"))
		for _, ref := range refs {
			parts.Write(pktLineStr(ref.oid + " " + ref.name + "\n"))
			if ref.peeled != "" {
				parts.Write(pktLineStr(ref.peeled + " " + ref.name + "^{}\n"))
			}
		}
	}
	parts.Write(pktFlush)
	buf := parts.Bytes()
	repo.v0AdFp = fpStr
	repo.v0AdBuf = buf
	return buf
}

// The v2 capability advertisement never changes: build it once, share it.
var v2AdvertisementBuf = func() []byte {
	var parts bytes.Buffer
	parts.Write(pktLineStr("version 2\n"))
	parts.Write(pktLineStr("agent=" + agentName + "\n"))
	parts.Write(pktLineStr("ls-refs=unborn\n"))
	parts.Write(pktLineStr("fetch=shallow filter\n"))
	parts.Write(pktLineStr("object-format=sha1\n"))
	parts.Write(pktFlush)
	return parts.Bytes()
}()

// ---------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------

func handleInfoRefs(req *Request, repo *RepoRecord, service string) Response {
	if repo == nil {
		return sendJSONResponse(404, map[string]any{"message": "repository not found"})
	}
	if service == "git-upload-pack" {
		if req.Header("git-protocol") == "version=2" {
			return sendBufferResponse(200, "application/x-git-upload-pack-advertisement", v2AdvertisementBuf)
		}
		return sendBufferResponse(200, "application/x-git-upload-pack-advertisement", v0Advertisement(repo, "git-upload-pack"))
	}
	if service == "git-receive-pack" {
		return sendBufferResponse(200, "application/x-git-receive-pack-advertisement", v0Advertisement(repo, "git-receive-pack"))
	}
	return sendJSONResponse(400, map[string]any{"message": "unknown service " + service})
}

// ---------------------------------------------------------------------------
// upload-pack v2: ls-refs
// ---------------------------------------------------------------------------

func handleLsRefsV2(repo *RepoRecord, argLines []string) []byte {
	args := make([]string, len(argLines))
	for i, l := range argLines {
		args[i] = strings.TrimSuffix(l, "\n")
	}
	peel := false
	symrefs := false
	var prefixes []string
	for _, line := range args {
		if line == "peel" {
			peel = true
		} else if line == "symrefs" {
			symrefs = true
		} else if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
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
	var parts bytes.Buffer
	refs := listRefs(repo)
	mainOid := repo.Refs["refs/heads/main"]
	if len(refs) == 0 {
		if keep("HEAD") {
			if symrefs {
				parts.Write(pktLineStr("unborn HEAD symref-target:refs/heads/main\n"))
			} else {
				parts.Write(pktLineStr("unborn HEAD\n"))
			}
		}
	} else {
		if mainOid != "" && keep("HEAD") {
			if symrefs {
				parts.Write(pktLineStr(mainOid + " HEAD symref-target:refs/heads/main\n"))
			} else {
				parts.Write(pktLineStr(mainOid + " HEAD\n"))
			}
		}
		for _, ref := range refs {
			if !keep(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if peel && ref.peeled != "" {
				line += " peeled:" + ref.peeled
			}
			parts.Write(pktLineStr(line + "\n"))
		}
	}
	parts.Write(pktFlush)
	return parts.Bytes()
}

// ---------------------------------------------------------------------------
// upload-pack: fetch (v2 and v0)
// ---------------------------------------------------------------------------

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	depth    int
	hasDepth bool
}

func parseFetchArgs(lines []string) fetchArgs {
	var args fetchArgs
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			oid := strings.SplitN(line[5:], " ", 2)[0]
			if isHex40(oid) {
				args.wants = append(args.wants, oid)
			}
		case strings.HasPrefix(line, "have "):
			oid := strings.SplitN(line[5:], " ", 2)[0]
			if isHex40(oid) {
				args.haves = append(args.haves, oid)
			}
		case strings.HasPrefix(line, "shallow "):
			oid := strings.SplitN(line[8:], " ", 2)[0]
			if isHex40(oid) {
				args.shallow = append(args.shallow, oid)
			}
		case strings.HasPrefix(line, "deepen "):
			var n int
			if _, err := fmt.Sscanf(line[7:], "%d", &n); err == nil && n > 0 {
				args.depth = n
				args.hasDepth = true
			}
		}
		// thin-pack, no-progress, ofs-delta, filter, done, deepen-since,
		// deepen-not, agent, object-format: accepted and ignored.
	}
	return args
}

// Fetch-response cache, per repo. A fetch's bytes are fully determined by
// (protocol version, wants, haves, shallow boundaries, depth) plus objects
// that never change content, so a cached response stays correct without
// invalidation — with one guard: a fetch is only cached when every want
// was present, so a fetch that answered "missing object" (empty pack) can
// never shadow the object arriving later. Bounded; clears when full.
const fetchCacheMax = 128

func fetchCacheKey(version int, args fetchArgs) string {
	return fmt.Sprintf("%d|%s|%s|%s|%d", version,
		strings.Join(args.wants, ","), strings.Join(args.haves, ","),
		strings.Join(args.shallow, ","), args.depth)
}

func fetchCacheGet(repo *RepoRecord, key string) []byte {
	if repo.fetchCache == nil {
		return nil
	}
	return repo.fetchCache[key]
}

func fetchCacheSet(repo *RepoRecord, key string, value []byte) {
	if repo.fetchCache == nil {
		repo.fetchCache = make(map[string][]byte)
	}
	if len(repo.fetchCache) >= fetchCacheMax {
		repo.fetchCache = make(map[string][]byte)
	}
	repo.fetchCache[key] = value
}

func handleFetchV2(repo *RepoRecord, args fetchArgs) []byte {
	cacheKey := fetchCacheKey(2, args)
	if cached := fetchCacheGet(repo, cacheKey); cached != nil {
		return cached
	}
	plan := planFetch(repo, args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
	pack := buildPack(repo, plan.send)
	var parts bytes.Buffer
	parts.Write(pktLineStr("acknowledgments\n"))
	parts.Write(pktLineStr("NAK\n"))
	parts.Write(pktDelim)
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		parts.Write(pktLineStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			parts.Write(pktLineStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			parts.Write(pktLineStr("unshallow " + oid + "\n"))
		}
		parts.Write(pktDelim)
	}
	parts.Write(pktLineStr("packfile\n"))
	parts.Write(sidebandPack(pack))
	out := parts.Bytes()
	allPresent := true
	for _, want := range args.wants {
		if getObject(repo, want) == nil {
			allPresent = false
			break
		}
	}
	if allPresent {
		fetchCacheSet(repo, cacheKey, out)
	}
	return out
}

func handleFetchV0(repo *RepoRecord, args fetchArgs) []byte {
	cacheKey := fetchCacheKey(0, args)
	if cached := fetchCacheGet(repo, cacheKey); cached != nil {
		return cached
	}
	plan := planFetch(repo, args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
	pack := buildPack(repo, plan.send)
	var parts bytes.Buffer
	parts.Write(pktLineStr("NAK\n"))
	for _, oid := range plan.shallow {
		parts.Write(pktLineStr("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		parts.Write(pktLineStr("unshallow " + oid + "\n"))
	}
	parts.Write(sidebandPack(pack))
	out := parts.Bytes()
	allPresent := true
	for _, want := range args.wants {
		if getObject(repo, want) == nil {
			allPresent = false
			break
		}
	}
	if allPresent {
		fetchCacheSet(repo, cacheKey, out)
	}
	return out
}

func handleUploadPack(req *Request, repo *RepoRecord) Response {
	body := req.Body
	decoded, _, err := decodePktLines(body, false)
	if err != nil {
		return sendJSONResponse(400, map[string]any{"message": "malformed pkt-line stream"})
	}
	isV2 := req.Header("git-protocol") == "version=2"
	if isV2 {
		lines := packetLines(decoded)
		commandLine := ""
		if len(lines) > 0 {
			commandLine = lines[0]
		}
		if !strings.HasPrefix(commandLine, "command=") {
			return sendJSONResponse(400, map[string]any{"message": "missing command"})
		}
		command := strings.TrimSuffix(commandLine[len("command="):], "\n")
		delimIndex := -1
		for i, p := range decoded {
			if p.kind == "delim" {
				delimIndex = i
				break
			}
		}
		var argPackets []decodedPacket
		if delimIndex >= 0 {
			argPackets = decoded[delimIndex+1:]
		} else if len(decoded) > 1 {
			argPackets = decoded[1:]
		}
		argLines := packetLines(argPackets)
		if command == "ls-refs" {
			return sendBufferResponse(200, "application/x-git-upload-pack-result", handleLsRefsV2(repo, argLines))
		}
		if command == "fetch" {
			args := parseFetchArgs(argLines)
			return sendBufferResponse(200, "application/x-git-upload-pack-result", handleFetchV2(repo, args))
		}
		return sendJSONResponse(400, map[string]any{"message": "unknown command " + command})
	}
	// v0: wants (+shallow/deepen), flush, haves, done, flush.
	lines := packetLines(decoded)
	args := parseFetchArgs(lines)
	return sendBufferResponse(200, "application/x-git-upload-pack-result", handleFetchV0(repo, args))
}

// ---------------------------------------------------------------------------
// receive-pack v0
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func parsePushCommands(lines []string) ([]pushCommand, []string, error) {
	var commands []pushCommand
	var capabilities []string
	for index, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		if line == "" {
			continue
		}
		rest := line
		if index == 0 {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				for _, token := range strings.Split(line[nul+1:], " ") {
					token = strings.TrimSpace(token)
					if token != "" {
						capabilities = append(capabilities, token)
					}
				}
				rest = line[:nul]
			}
		}
		parts := strings.Split(rest, " ")
		if len(parts) < 3 {
			return nil, nil, fmt.Errorf("bad push command %q", line[:minInt(60, len(line))])
		}
		oldOid, newOid, ref := parts[0], parts[1], parts[2]
		if !isHex40(oldOid) || !isHex40(newOid) || !strings.HasPrefix(ref, "refs/") {
			return nil, nil, fmt.Errorf("bad push command %q", line[:minInt(60, len(line))])
		}
		commands = append(commands, pushCommand{oldOid: oldOid, newOid: newOid, ref: ref})
	}
	return commands, capabilities, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type pushResult struct {
	ref    string
	ok     bool
	reason string
}

func pushReport(unpackOk bool, unpackDetail string, results []pushResult, sideband bool) []byte {
	var inner bytes.Buffer
	inner.Write(pktLineStr("unpack " + unpackDetail + "\n"))
	for _, r := range results {
		if r.ok {
			inner.Write(pktLineStr("ok " + r.ref + "\n"))
		} else {
			inner.Write(pktLineStr("ng " + r.ref + " " + r.reason + "\n"))
		}
	}
	inner.Write(pktFlush)
	innerBuf := inner.Bytes()
	if !sideband {
		return innerBuf
	}
	var parts bytes.Buffer
	for offset := 0; offset < len(innerBuf); offset += sidebandChunk {
		end := offset + sidebandChunk
		if end > len(innerBuf) {
			end = len(innerBuf)
		}
		payload := make([]byte, 1+end-offset)
		payload[0] = 1
		copy(payload[1:], innerBuf[offset:end])
		parts.Write(pktLine(payload))
	}
	parts.Write(pktFlush)
	return parts.Bytes()
}

func handleReceivePack(req *Request, repo *RepoRecord) Response {
	body := req.Body
	var commands []pushCommand
	var capabilities []string
	var packOffset int
	parseErr := func() error {
		decoded, consumed, err := decodePktLines(body, true)
		if err != nil {
			return err
		}
		flushIndex := -1
		for i, p := range decoded {
			if p.kind == "flush" {
				flushIndex = i
				break
			}
		}
		if flushIndex < 0 {
			return fmt.Errorf("push command list has no flush")
		}
		parsed, caps, err := parsePushCommands(packetLines(decoded[:flushIndex]))
		if err != nil {
			return err
		}
		if len(parsed) == 0 {
			return fmt.Errorf("push needs at least one command")
		}
		commands = parsed
		capabilities = caps
		packOffset = consumed
		return nil
	}()
	if parseErr != nil {
		return sendJSONResponse(400, map[string]any{"message": parseErr.Error()})
	}
	sideband := false
	for _, c := range capabilities {
		if c == "side-band-64k" {
			sideband = true
		}
	}
	packBytes := body[packOffset:]
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, func(oid string) *GitObject { return getObject(repo, oid) })
		if err != nil {
			unpackDetail = "unpacker error"
		} else {
			for oid, obj := range objects {
				repo.Objects[oid] = obj
			}
		}
	}
	var results []pushResult
	for _, cmd := range commands {
		if unpackDetail != "ok" {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "unpacker error"})
			continue
		}
		current, exists := repo.Refs[cmd.ref]
		if cmd.newOid == zeroOID {
			// Delete.
			if !exists {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else if cmd.oldOid != zeroOID && cmd.oldOid != current {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "old value does not match"})
			} else {
				delete(repo.Refs, cmd.ref)
				results = append(results, pushResult{ref: cmd.ref, ok: true})
			}
			continue
		}
		if !exists {
			// Create.
			if cmd.oldOid != zeroOID {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
			if !isComplete(repo, cmd.newOid) {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "missing object"})
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOid
			noteReachable(repo, cmd.newOid)
			results = append(results, pushResult{ref: cmd.ref, ok: true})
			continue
		}
		// Update.
		if cmd.oldOid == zeroOID {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "already exists"})
		} else if cmd.oldOid != current {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "old value does not match"})
		} else if cmd.newOid == current {
			results = append(results, pushResult{ref: cmd.ref, ok: true})
		} else {
			if !isComplete(repo, cmd.newOid) {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "missing object"})
			} else if strings.HasPrefix(cmd.ref, "refs/heads/") && !isAncestor(repo, current, cmd.newOid) {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				repo.Refs[cmd.ref] = cmd.newOid
				noteReachable(repo, cmd.newOid)
				results = append(results, pushResult{ref: cmd.ref, ok: true})
			}
		}
	}
	report := pushReport(unpackDetail == "ok", unpackDetail, results, sideband)
	return sendBufferResponse(200, "application/x-git-receive-pack-result", report)
}

// ---------------------------------------------------------------------------
// entry point: the caller routes only /{owner}/{repo}.git/* paths here.
// ---------------------------------------------------------------------------

func handleGit(req *Request, repo *RepoRecord) (Response, bool) {
	path := req.Path
	if strings.HasSuffix(path, "/info/refs") {
		if req.Method != "GET" {
			return sendJSONResponse(405, map[string]any{"message": "method not allowed"}), true
		}
		service := ""
		if req.Query != nil {
			service = req.Query.Get("service")
		}
		return handleInfoRefs(req, repo, service), true
	}
	if strings.HasSuffix(path, "/git-upload-pack") {
		if req.Method != "POST" {
			return sendJSONResponse(405, map[string]any{"message": "method not allowed"}), true
		}
		if repo == nil {
			return sendJSONResponse(404, map[string]any{"message": "repository not found"}), true
		}
		return handleUploadPack(req, repo), true
	}
	if strings.HasSuffix(path, "/git-receive-pack") {
		if req.Method != "POST" {
			return sendJSONResponse(405, map[string]any{"message": "method not allowed"}), true
		}
		if repo == nil {
			return sendJSONResponse(404, map[string]any{"message": "repository not found"}), true
		}
		return handleReceivePack(req, repo), true
	}
	return Response{}, false
}
