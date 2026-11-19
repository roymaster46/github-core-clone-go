// The forge's HTTP surface: routes every request to the module that
// owns it. Port of the Node entry's app.ts.
//
// One global mutex serialises store access, mirroring Node's single
// thread: the handlers were written against a store no other request
// mutates mid-flight (learned owners, caches, number sequences), and
// the only concurrent workload is 50 shallow clones whose fetch
// responses are cached after the first, so serialisation costs
// nothing measurable there.

package main

import (
	"net/url"
	"regexp"
	"strings"
	"sync"
)

var appMu sync.Mutex

func json404() Response {
	b, _ := marshalJSON(map[string]any{"message": "Not Found"})
	return Response{Status: 404, Headers: [][2]string{{"Content-Type", "application/json; charset=utf-8"}}, Body: append(b, '\n')}
}

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

func createHandler(store *Store) func(*Request) Response {
	return func(req *Request) (resp Response) {
		defer func() {
			if r := recover(); r != nil {
				b, _ := marshalJSON(map[string]any{"message": "Internal Server Error"})
				resp = Response{Status: 500, Headers: [][2]string{{"Content-Type", "application/json; charset=utf-8"}}, Body: append(b, '\n')}
			}
		}()
		appMu.Lock()
		defer appMu.Unlock()

		path := req.Path
		method := req.Method

		// The env's boot check calls this twice: 200 {"ok":true}. No auth.
		if method == "GET" && path == "/healthz" {
			b, _ := marshalJSON(map[string]any{"ok": true})
			return Response{Status: 200, Headers: [][2]string{{"Content-Type", "application/json; charset=utf-8"}}, Body: append(b, '\n')}
		}

		// REST v3 (includes GET /api/v3/ root and /api/v3/meta).
		if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
			if resp, ok := handleRest(req, store); ok {
				return resp
			}
		}
		// GraphQL v4.
		if path == "/api/graphql" {
			if resp, ok := handleGraphql(req, store); ok {
				return resp
			}
		}
		// MCP over streamable HTTP.
		if path == "/mcp" {
			if resp, ok := handleMcp(req, store); ok {
				return resp
			}
		}
		// Git smart HTTP: /{owner}/{repo}.git/info/refs,
		// /git-upload-pack, /git-receive-pack.
		if m := gitRouteRe.FindStringSubmatch(path); m != nil {
			owner, _ := url.PathUnescape(m[1])
			repoName, _ := url.PathUnescape(m[2])
			repo := store.ResolveRepo(owner, repoName)
			if repo == nil {
				return json404()
			}
			if resp, ok := handleGit(req, repo); ok {
				return resp
			}
		}

		// Everything else: GitHub's 404 body.
		return json404()
	}
}
