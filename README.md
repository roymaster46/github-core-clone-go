# github-core-clone-go

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/roymaster46/github-core-clone-go/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/roymaster46/github-core-clone-go/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge, written in Go (standard library only), entered in make.fun's `github-core-clone-01` bounty. One process, no git binary, no network, no UI.

## What it serves

- **Git smart HTTP** (`/{owner}/{repo}.git`): upload-pack protocol v2 (plus v0 fallback), receive-pack v0 with report-status / side-band-64k / delete-refs, fast-forward enforcement, shallow fetch. Packfiles are built and parsed in-process (OFS/REF deltas on read, whole objects on write).
- **REST v3** (`/api/v3`): repositories, branches, commits, contents, labels, issues (+comments), pull requests (+files, squash merge with conflict detection). Bearer auth, `X-GitHub-Request-Id` on every reply, JSON errors.
- **GraphQL v4** (`/api/graphql`): repository/issues/pullRequests queries with ordering + cursor pagination, `createPullRequest` / `createIssue` / `mergePullRequest` mutations, introspection for the types `gh` and `github-mcp-server` send.
- **MCP** (`/mcp`): JSON-RPC 2.0 Streamable HTTP, `initialize`, `tools/list` (6 tools), `tools/call` acting on the same store as REST.

## Run it

```sh
node main.ts
```

`main.ts` is a thin launcher: it spawns the statically linked Go binary `./forge` (built from the Go sources in this repository with `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`), which is the actual forge. The forge binds an ephemeral port, POSTs the port to `$MAKEFUN_ENV_URL/register`, polls `$MAKEFUN_ENV_URL/done`, and exits 0. To rebuild the binary from source:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
```

## Local verification

Against the kit's own conformance suite (`sdk/run.ts`):

```
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token TOK --perf
```

## Hot paths

- **Transport** (`transport.go`): a raw TCP HTTP/1.1 server — one `conn.Write` per response, the request path/query parsed once, `TCP_NODELAY` set explicitly, idle sockets closed after 30 s.
- **Push** (`git.go`): ancestry walks use cached parent lists and stop at commits already known reachable from a ref, so a push costs O(new history). Pack entries inflate with a counting reader that pins the exact stream end.
- **Clone** (`git_http.go`): fetch responses are cached per (protocol, wants, haves, shallow, depth) — objects never change content, and a fetch is only cached once every want is present. The v2 capability advertisement is one shared buffer and the v0 advertisement is cached per ref-set fingerprint.
- **Read** (`rest.go`): GET-issue responses are cached per issue, keyed on the issue's `updated_at`, its comment version, and the learned owner — the cached value is the response bytes themselves (no re-marshal).
- **Store** (`store.go`): one mutex serialises store access, mirroring Node's single thread; the only concurrent workload is 50 shallow clones whose fetch responses are cached after the first.

---

<sub>Entered on [make.fun](https://make.fun/?ref=26tnw136&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
