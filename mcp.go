// MCP endpoint (POST /mcp) for the github-core-clone-01 Go forge.
// JSON-RPC 2.0 over Streamable HTTP, plain-JSON answers. Port of the
// Node entry's src/mcp.ts. Speaks the modern revision 2026-07-28
// (server/discover) and the legacy handshake (initialize +
// notifications/initialized) alike. All tools act on the same Store,
// reusing the shared helpers from graphql.go so numbering, merge
// semantics and object shapes match REST/GraphQL.

package main

import (
	"fmt"
	"strconv"
	"strings"
)

type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

var mcpTools = []toolDef{
	{
		Name: "create_repo", Description: "Create a new repository owned by the authenticated user.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Repository name."},
				"description": map[string]any{"type": "string", "description": "Repository description."},
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			},
			"required": []any{"name"},
		},
	},
	{
		Name: "get_repo", Description: "Get a repository by owner and name.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "Full repository name \"owner/repo\"."},
			},
			"required": []any{"owner", "repo"},
		},
	},
	{
		Name: "create_issue", Description: "Create an issue in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "Full repository name \"owner/repo\"."},
				"title":     map[string]any{"type": "string", "description": "Issue title."},
				"body":      map[string]any{"type": "string", "description": "Issue body."},
			},
			"required": []any{"owner", "repo", "title"},
		},
	},
	{
		Name: "list_issues", Description: "List issues in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "Full repository name \"owner/repo\"."},
				"state":     map[string]any{"type": "string", "description": "Issue state filter: open, closed, or all."},
			},
			"required": []any{"owner", "repo"},
		},
	},
	{
		Name: "create_pull_request", Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "Full repository name \"owner/repo\"."},
				"title":     map[string]any{"type": "string", "description": "Pull request title."},
				"body":      map[string]any{"type": "string", "description": "Pull request body."},
				"head":      map[string]any{"type": "string", "description": "Head branch name."},
				"base":      map[string]any{"type": "string", "description": "Base branch name."},
			},
			"required": []any{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name: "merge_pull_request", Description: "Squash-merge a pull request into its base branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":        map[string]any{"type": "string", "description": "Repository name."},
				"full_name":   map[string]any{"type": "string", "description": "Full repository name \"owner/repo\"."},
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
			},
			"required": []any{"owner", "repo", "pull_number"},
		},
	},
}

var supportedVersions = []any{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
var mcpServerInfo = map[string]any{"name": "github-forge", "version": "1.0.0"}
var mcpCapabilities = map[string]any{"tools": map[string]any{}}

// mcpToolsJSON is built once, reused for every tools/list answer so two
// calls are byte-identical.
var mcpToolsJSON = func() []any {
	out := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	return out
}()

func mcpAuthed(req *Request) bool { return gqlAuthed(req) }

func mcpSendJSON(status int, body any) Response {
	b, err := marshalJSON(body)
	if err != nil {
		b = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
	}
	return Response{Status: status, Headers: [][2]string{{"Content-Type", "application/json"}}, Body: b}
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func toolText(obj any) map[string]any {
	var text string
	if s, ok := obj.(string); ok {
		text = s
	} else {
		b, _ := marshalJSON(obj)
		text = string(b)
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}
}

func toolError(message string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}, "isError": true}
}

func isRecord(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// resolveOwnerRepo resolves owner/repo from arguments, accepting
// full_name "owner/repo".
func resolveOwnerRepo(args map[string]any) (owner, repo string, ok bool) {
	owner = asString(args["owner"])
	repo = asString(args["repo"])
	if owner == "" {
		owner = asString(args["owner_login"])
	}
	if repo == "" {
		repo = asString(args["repository"])
	}
	if owner == "" || repo == "" {
		if full, isStr := args["full_name"].(string); isStr {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				if owner == "" {
					owner = parts[0]
				}
				if repo == "" {
					repo = parts[1]
				}
			}
		}
	}
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func getRepoOrError(store *Store, args map[string]any) (*RepoRecord, string, string, string) {
	owner, repo, ok := resolveOwnerRepo(args)
	if !ok {
		return nil, "", "", "Missing required argument: owner and repo (or full_name \"owner/repo\")."
	}
	found := store.ResolveRepo(owner, repo)
	if found == nil {
		return nil, "", "", "Repository not found: " + owner + "/" + repo
	}
	return found, owner, repo, ""
}

var mcpRequired = map[string][]string{
	"create_repo":         {"name"},
	"get_repo":            {"owner", "repo"},
	"create_issue":        {"owner", "repo", "title"},
	"list_issues":         {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request":  {"owner", "repo", "pull_number"},
}

func missingArgs(name string, args map[string]any) []string {
	required := mcpRequired[name]
	var missing []string
	for _, key := range required {
		if key == "owner" || key == "repo" {
			// full_name can stand in for owner+repo
			if full, ok := args["full_name"].(string); ok && strings.Contains(full, "/") {
				continue
			}
		}
		v := args[key]
		if v == nil || v == "" {
			missing = append(missing, key)
		}
	}
	return missing
}

func callTool(store *Store, name string, args map[string]any) map[string]any {
	actor := store.TokenOwner
	if missing := missingArgs(name, args); len(missing) > 0 {
		return toolError("Missing required argument(s): " + strings.Join(missing, ", "))
	}

	switch name {
	case "create_repo":
		repoName := fmt.Sprint(args["name"])
		owner := actor
		if store.GetRepo(owner, repoName) != nil {
			return toolError("Repository already exists: " + owner + "/" + repoName)
		}
		repo := store.CreateRepo(owner, repoName, asString(args["description"]))
		return toolText(restRepoObject(store, repo))
	case "get_repo":
		found, _, _, errStr := getRepoOrError(store, args)
		if errStr != "" {
			return toolError(errStr)
		}
		return toolText(restRepoObject(store, found))
	case "create_issue":
		found, _, _, errStr := getRepoOrError(store, args)
		if errStr != "" {
			return toolError(errStr)
		}
		issue := createIssueRecord(store, found, fmt.Sprint(args["title"]), asString(args["body"]), actor)
		return toolText(restIssueObject(found, issue))
	case "list_issues":
		found, _, _, errStr := getRepoOrError(store, args)
		if errStr != "" {
			return toolError(errStr)
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = strings.ToLower(s)
		}
		var list []any
		// Sort by number ascending: collect then sort.
		var issues []*IssueRecord
		for _, i := range found.Issues {
			issues = append(issues, i)
		}
		sortIssuesByNumber(issues)
		for _, i := range issues {
			keep := false
			switch state {
			case "all":
				keep = true
			case "closed":
				keep = i.State == "closed"
			default:
				keep = i.State == "open"
			}
			if keep {
				list = append(list, restIssueObject(found, i))
			}
		}
		if list == nil {
			list = []any{}
		}
		return toolText(list)
	case "create_pull_request":
		found, _, _, errStr := getRepoOrError(store, args)
		if errStr != "" {
			return toolError(errStr)
		}
		pull := createPullRecord(found, fmt.Sprint(args["title"]), asString(args["body"]), fmt.Sprint(args["head"]), fmt.Sprint(args["base"]), actor)
		if pull == nil {
			return toolError("Head branch does not exist: " + fmt.Sprint(args["head"]))
		}
		return toolText(restPullObject(found, pull))
	case "merge_pull_request":
		found, _, _, errStr := getRepoOrError(store, args)
		if errStr != "" {
			return toolError(errStr)
		}
		numRaw := args["pull_number"]
		if numRaw == nil {
			numRaw = args["number"]
		}
		if numRaw == nil {
			numRaw = args["pull_request_number"]
		}
		num := 0
		switch n := numRaw.(type) {
		case float64:
			num = int(n)
		case int:
			num = n
		case string:
			v, err := strconv.Atoi(n)
			if err != nil {
				return toolError("Missing required argument: pull_number must be an integer.")
			}
			num = v
		default:
			return toolError("Missing required argument: pull_number must be an integer.")
		}
		pull, ok := found.Pulls[num]
		if !ok {
			return toolError("Pull request not found: #" + strconv.Itoa(num))
		}
		sha, errStr := squashMergePull(found, pull, actor)
		if errStr != "" {
			return toolError("Cannot merge pull request #" + strconv.Itoa(num) + ": " + errStr)
		}
		return toolText(map[string]any{"sha": sha, "merged": true, "message": "Pull request #" + strconv.Itoa(num) + " merged."})
	default:
		return toolError("Unknown tool: " + name)
	}
}

func handleMcp(req *Request, store *Store) (Response, bool) {
	if req.Path != "/mcp" {
		return Response{}, false
	}
	if req.Method == "DELETE" {
		// Legacy session teardown (client.close()): acknowledge, nothing held.
		return Response{Status: 202}, true
	}
	if req.Method != "POST" {
		return mcpSendJSON(405, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": "method not allowed"}}), true
	}
	if !mcpAuthed(req) {
		return mcpSendJSON(401, map[string]any{"message": "unauthorized"}), true
	}
	var body any
	if err := jsonUnmarshal(req.Body, &body); err != nil {
		return mcpSendJSON(400, rpcError(nil, -32700, "Parse error")), true
	}
	record, ok := isRecord(body)
	if !ok {
		return mcpSendJSON(400, rpcError(nil, -32600, "Invalid Request: expected a JSON-RPC object")), true
	}
	id := record["id"]
	method, _ := record["method"].(string)
	params, _ := isRecord(record["params"])

	// Notifications (no id) get no JSON-RPC response: 202 empty body.
	if id == nil {
		if strings.HasPrefix(method, "notifications/") {
			return Response{Status: 202}, true
		}
		if method == "" {
			return mcpSendJSON(400, rpcError(nil, -32600, "Invalid Request")), true
		}
	}

	switch method {
	case "server/discover":
		return mcpSendJSON(200, rpcResult(id, map[string]any{
			"resultType":        "server/discover",
			"supportedVersions": supportedVersions,
			"capabilities":      mcpCapabilities,
			"serverInfo":        mcpServerInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})), true
	case "initialize":
		requested := asString(params["protocolVersion"])
		version := "2026-07-28"
		for _, v := range supportedVersions {
			if v == requested {
				version = requested
				break
			}
		}
		return mcpSendJSON(200, rpcResult(id, map[string]any{
			"protocolVersion": version,
			"capabilities":    mcpCapabilities,
			"serverInfo":      mcpServerInfo,
		})), true
	case "ping":
		return mcpSendJSON(200, rpcResult(id, map[string]any{})), true
	case "tools/list":
		// Same object every time: byte-identical across calls.
		return mcpSendJSON(200, rpcResult(id, map[string]any{"tools": mcpToolsJSON})), true
	case "tools/call":
		toolName := asString(params["name"])
		args, _ := isRecord(params["arguments"])
		if args == nil {
			args = map[string]any{}
		}
		known := false
		for _, t := range mcpTools {
			if t.Name == toolName {
				known = true
				break
			}
		}
		if toolName == "" || !known {
			missing := toolName
			if missing == "" {
				missing = "(missing name)"
			}
			return mcpSendJSON(200, rpcError(id, -32601, "Method not found: tool "+missing)), true
		}
		var result map[string]any
		func() {
			defer func() {
				if r := recover(); r != nil {
					result = toolError(fmt.Sprint(r))
				}
			}()
			result = callTool(store, toolName, args)
		}()
		return mcpSendJSON(200, rpcResult(id, result)), true
	default:
		return mcpSendJSON(200, rpcError(id, -32601, "Method not found: "+method)), true
	}
}
