package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Configuring Sonda used to be a person's job in a browser. These tools hand
// it to the agent, so "connect this project to Sonda" is one sentence rather
// than twenty-one form entries.
//
// There is one thing they cannot do, and it shapes all of them. Sonda is an
// explicit proxy: it sees nothing until whoever makes the call is pointed at
// it, and Sonda cannot repoint anyone. The agent can — it has the filesystem
// and it can restart a process. So the division is:
//
//	Sonda knows the mapping. The agent has the hands.
//
// connect_project therefore does not just configure; it hands back the exact
// edit to apply. And disconnect_project hands back its inverse, because an
// agent that leaves a .env pointing at a Sonda that is no longer listening has
// broken the environment it was asked to help with.

// changing marks a tool that alters what is listening. Creating configuration
// disturbs nobody; opening and closing ports can pull the floor out from under
// someone mid-debug, so the client is asked first.
var changing = map[string]any{
	"readOnlyHint":    false,
	"destructiveHint": true,
	"idempotentHint":  true,
	"openWorldHint":   false,
}

// writing marks a tool that stores configuration without touching a socket.
var writing = map[string]any{
	"readOnlyHint":    false,
	"destructiveHint": false,
	"idempotentHint":  false,
	"openWorldHint":   false,
}

func configureTools() []Tool {
	return []Tool{
		{
			Name:  "connect_project",
			Title: "Connect a project to Sonda",
			Description: "Set Sonda up to observe a whole system in one step. Give it the contents of the project's own configuration — a .env full of *_URL entries, or a compose file — and it finds the services, assigns a proxy port to each, and creates the project. " +
				"It returns the exact variable changes to apply so the traffic starts flowing through Sonda: that part is yours, because Sonda cannot repoint a caller. It does not activate the project; call activate_project when you are ready for the ports to open.",
			Schema: obj(map[string]any{
				"name": prop("string", "Name for the project, for example the repository name."),
				"files": map[string]any{
					"type":        "array",
					"description": "The configuration files you already read. Sonda does not touch your disk; pass the contents.",
					"items": obj(map[string]any{
						"filename": prop("string", "Name of the file, used to tell a .env from a compose file."),
						"content":  prop("string", "Its contents."),
					}, "filename", "content"),
				},
			}, "name", "files"),
			Annotations: writing,
			Run:         connectProject,
		},

		{
			Name:        "configure_service",
			Title:       "Add or change a service",
			Description: "Add a service to a project, or change one that is already there. Use it to fix a port Sonda could not take, or to add something the configuration file did not mention.",
			Schema: obj(map[string]any{
				"project":  prop("string", "Project name."),
				"name":     prop("string", "Service name, as you want to see it."),
				"listen":   prop("string", "Address Sonda should listen on, like 127.0.0.1:9152."),
				"upstream": prop("string", "Where the real service is, like http://localhost:50052."),
				"protocol": map[string]any{"type": "string", "enum": []string{"http", "grpc"}, "description": "Defaults to http."},
			}, "project", "name", "listen", "upstream"),
			Annotations: writing,
			Run:         configureService,
		},

		{
			Name:        "activate_project",
			Title:       "Open a project's ports",
			Description: "Make this the project Sonda is observing: closes whatever was listening and opens this project's ports. Only one project listens at a time. This changes what is running, so expect to be asked first.",
			Schema: obj(map[string]any{
				"project": prop("string", "Project name."),
			}, "project"),
			Annotations: changing,
			Run:         activateProject,
		},

		{
			Name:  "set_stub",
			Title: "Answer for a service from recordings",
			Description: "Stop forwarding to a service and answer from what Sonda already recorded of it. Use it to work while that service is down, to make a test deterministic, or to reproduce something without the environment that produced it. " +
				"A request with no recording gets a 501 saying so rather than an invented answer. Every stubbed answer carries an X-Sonda-Stub header, and stubbing is forgotten when Sonda restarts.",
			Schema: obj(map[string]any{
				"service": prop("string", "Service name. Leave it out together with clear:true to put everything back to live traffic."),
				"enabled": prop("boolean", "True to answer from recordings, false to forward again."),
				"clear":   prop("boolean", "Put every service back to live traffic at once."),
			}),
			// Not destructive in the sense of deleting anything, but a service
			// quietly answering from last week is exactly the surprise worth
			// asking about first.
			Annotations: changing,
			Run:         setStub,
		},

		{
			Name:  "break_service",
			Title: "Make a service misbehave on purpose",
			Description: "Add latency, answer with a status without reaching the service, or cut the connection outright. Use it to find out whether the caller's retries, timeouts and degradation actually work — the code nobody exercises because making a real service fail is awkward. " +
				"Faults are deterministic: one call in three means one call in three, the same sequence every run, so a failure can be reproduced. Every injected failure is recorded as injected and carries an X-Sonda-Fault header, and rules are forgotten when Sonda restarts.",
			Schema: obj(map[string]any{
				"service":    prop("string", "Service name."),
				"latency_ms": prop("integer", "Milliseconds to add before forwarding. The service still answers."),
				"status":     prop("integer", "Answer with this HTTP status instead of forwarding. The service is never reached."),
				"cut":        prop("boolean", "Drop the connection without answering at all."),
				"one_in":     prop("integer", "Apply to one call in every N. Defaults to every call."),
				"clear":      prop("boolean", "Remove the rule for this service."),
				"clear_all":  prop("boolean", "Remove every rule."),
			}),
			// It changes what callers receive, which is exactly the surprise
			// worth confirming first.
			Annotations: changing,
			Run:         breakService,
		},

		{
			Name:  "disconnect_project",
			Title: "Close the ports and undo the pointing",
			Description: "Stop observing: closes every port and returns the variable changes that put things back the way they were, so whatever you repointed can be pointed at the real services again. " +
				"Nothing is deleted — the project, its services and its schemas stay, and activate_project brings them back.",
			Schema:      obj(map[string]any{}),
			Annotations: changing,
			Run:         disconnectProject,
		},
	}
}

func connectProject(ctx context.Context, s *Server, a args) (any, error) {
	name := a.str("name")
	if name == "" {
		return nil, fmt.Errorf("the project needs a name")
	}

	rawFiles, _ := a["files"].([]any)
	if len(rawFiles) == 0 {
		return nil, fmt.Errorf("pass at least one configuration file, with its contents")
	}

	type discovered struct {
		Name      string `json:"name"`
		Upstream  string `json:"upstream"`
		Protocol  string `json:"protocol"`
		Listen    string `json:"listen"`
		Key       string `json:"key"`
		Original  string `json:"original"`
		PortTaken bool   `json:"port_taken"`
		PortError string `json:"port_error"`
	}

	// Read every file first. A service named in two of them is one service,
	// and the first reading wins — re-adding it would only fail on the unique
	// constraint further down and turn a duplicate into an error.
	found := map[string]discovered{}
	order := []string{}
	for _, entry := range rawFiles {
		file, _ := entry.(map[string]any)
		filename, _ := file["filename"].(string)
		content, _ := file["content"].(string)
		if content == "" {
			continue
		}

		payload, err := s.raw(ctx, "POST", "/api/discover?filename="+url.QueryEscape(filename), []byte(content))
		if err != nil {
			return nil, err
		}
		var body struct {
			Found []discovered `json:"found"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, err
		}
		for _, f := range body.Found {
			if _, seen := found[f.Name]; seen {
				continue
			}
			found[f.Name] = f
			order = append(order, f.Name)
		}
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("no services found in those files — Sonda looks for entries like MS_AUTH_GRPC_URL=localhost:50052, or published ports in a compose file")
	}

	created, err := s.raw(ctx, "POST", "/api/projects", []byte(`{"name":`+quote(name)+`}`))
	if err != nil {
		return nil, err
	}
	var project struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created, &project); err != nil {
		return nil, err
	}

	changes := map[string]any{}
	conflicts := []map[string]string{}
	added := 0

	for _, key := range order {
		f := found[key]
		service, err := json.Marshal(map[string]any{
			"name": f.Name, "listen": f.Listen, "upstream": f.Upstream, "protocol": f.Protocol,
		})
		if err != nil {
			return nil, err
		}
		if _, err := s.raw(ctx, "POST", fmt.Sprintf("/api/projects/%d/services", project.ID), service); err != nil {
			// One bad entry must not lose the other twenty. It is reported
			// beside the service it belongs to instead.
			conflicts = append(conflicts, map[string]string{"service": f.Name, "problem": err.Error()})
			continue
		}
		added++

		if f.PortTaken {
			conflicts = append(conflicts, map[string]string{
				"service": f.Name,
				"listen":  f.Listen,
				"problem": "that port is already in use, so it will not open — move it with configure_service",
			})
		}
		if f.Key != "" {
			changes[f.Key] = map[string]string{"from": f.Original, "to": f.Listen}
		}
	}

	out := map[string]any{
		"project":    name,
		"services":   added,
		"active":     false,
		"changes":    changes,
		"next_steps": "Apply the changes above to the file they came from, call activate_project, then restart whatever makes these calls so it picks up the new addresses. Sonda cannot repoint a caller by itself.",
	}
	if len(conflicts) > 0 {
		out["problems"] = conflicts
	}
	return out, nil
}

func configureService(ctx context.Context, s *Server, a args) (any, error) {
	id, err := projectID(ctx, s, a.str("project"))
	if err != nil {
		return nil, err
	}

	protocol := a.str("protocol")
	if protocol == "" {
		protocol = "http"
	}
	body, err := json.Marshal(map[string]any{
		"name": a.str("name"), "listen": a.str("listen"),
		"upstream": a.str("upstream"), "protocol": protocol, "reflection": true,
	})
	if err != nil {
		return nil, err
	}
	return s.post(ctx, fmt.Sprintf("/api/projects/%d/services", id), body)
}

func activateProject(ctx context.Context, s *Server, a args) (any, error) {
	id, err := projectID(ctx, s, a.str("project"))
	if err != nil {
		return nil, err
	}
	out, err := s.post(ctx, fmt.Sprintf("/api/projects/%d/activate", id), nil)
	if err != nil {
		return nil, err
	}
	// The answer is the whole project list, which is more than was asked and
	// buries the one thing that matters: whether the ports actually opened.
	return summarise(out, "activated"), nil
}

func disconnectProject(ctx context.Context, s *Server, a args) (any, error) {
	before, err := s.get(ctx, "/api/projects", false)
	if err != nil {
		return nil, err
	}

	if _, err := s.post(ctx, "/api/projects/deactivate", nil); err != nil {
		return nil, err
	}

	return map[string]any{
		"listening":  0,
		"changes":    reversePatch(before),
		"next_steps": "Put the changes above back into the configuration file and restart, so the callers talk to the real services again. Nothing was deleted: activate_project brings the project back exactly as it was.",
	}, nil
}

// reversePatch rebuilds the edit that undoes connect_project.
//
// Nothing extra is stored to make this work: the upstream is on every service
// and the variable name follows from the service name and its protocol, which
// is the same rule discovery used to read them apart in the first place.
func reversePatch(projects any) map[string]any {
	changes := map[string]any{}
	list, _ := projects.(map[string]any)["projects"].([]any)

	for _, entry := range list {
		project, _ := entry.(map[string]any)
		if active, _ := project["active"].(bool); !active {
			continue
		}
		services, _ := project["services"].([]any)
		for _, item := range services {
			service, _ := item.(map[string]any)
			name, _ := service["name"].(string)
			listen, _ := service["listen"].(string)
			upstream, _ := service["upstream"].(string)
			protocol, _ := service["protocol"].(string)
			if name == "" {
				continue
			}

			variable := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
			if protocol == "grpc" {
				variable += "_GRPC_URL"
			} else {
				variable += "_URL"
			}
			changes[variable] = map[string]string{
				"from": listen,
				"to":   strings.TrimPrefix(strings.TrimPrefix(upstream, "http://"), "https://"),
			}
		}
	}
	return changes
}

// summarise turns the project listing every mutation answers with into the
// part that was actually asked about.
func summarise(projects any, did string) map[string]any {
	list, _ := projects.(map[string]any)["projects"].([]any)
	for _, entry := range list {
		project, _ := entry.(map[string]any)
		if active, _ := project["active"].(bool); !active {
			continue
		}
		services, _ := project["services"].([]any)
		listening, failed := 0, []map[string]string{}
		for _, item := range services {
			service, _ := item.(map[string]any)
			if running, _ := service["running"].(bool); running {
				listening++
				continue
			}
			name, _ := service["name"].(string)
			problem, _ := service["error"].(string)
			failed = append(failed, map[string]string{"service": name, "problem": problem})
		}
		out := map[string]any{
			"project":   project["name"],
			did:         true,
			"listening": listening,
			"of":        len(services),
		}
		if len(failed) > 0 {
			// One busy port must not read as a failed activation: the other
			// twenty are up and observing.
			out["did_not_open"] = failed
		}
		return out
	}
	return map[string]any{did: true, "listening": 0}
}

// projectID resolves a name to an id, so every tool takes the name a person
// would say rather than a number they would have to look up.
func projectID(ctx context.Context, s *Server, name string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("which project? pass its name")
	}
	payload, err := s.raw(ctx, "GET", "/api/projects", nil)
	if err != nil {
		return 0, err
	}
	var body struct {
		Projects []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return 0, err
	}

	known := make([]string, 0, len(body.Projects))
	for _, p := range body.Projects {
		if strings.EqualFold(p.Name, name) {
			return p.ID, nil
		}
		known = append(known, p.Name)
	}
	if len(known) == 0 {
		return 0, fmt.Errorf("there is no project called %q, and none exist yet — connect_project creates one", name)
	}
	return 0, fmt.Errorf("there is no project called %q. There is: %s", name, strings.Join(known, ", "))
}

func quote(s string) string { return strconv.Quote(s) }

func setStub(ctx context.Context, s *Server, a args) (any, error) {
	if a.boolean("clear") {
		return s.post(ctx, "/api/stub", []byte(`{"clear":true}`))
	}
	service := a.str("service")
	if service == "" {
		return nil, fmt.Errorf("which service? pass its name, or clear:true to put everything back")
	}
	if !a.has("enabled") {
		return nil, fmt.Errorf("pass enabled:true to answer from recordings, or enabled:false to forward again")
	}
	body, err := json.Marshal(map[string]any{"service": service, "enabled": a.boolean("enabled")})
	if err != nil {
		return nil, err
	}
	return s.post(ctx, "/api/stub", body)
}

func breakService(ctx context.Context, s *Server, a args) (any, error) {
	if a.boolean("clear_all") {
		return s.post(ctx, "/api/faults", []byte(`{"clear_all":true}`))
	}
	service := a.str("service")
	if service == "" {
		return nil, fmt.Errorf("which service? pass its name, or clear_all:true to remove every rule")
	}

	rule := map[string]any{"service": service}
	if a.boolean("clear") {
		rule["clear"] = true
	} else {
		for _, key := range []string{"latency_ms", "status", "one_in"} {
			if a.has(key) {
				rule[key] = a.num(key, 0)
			}
		}
		if a.boolean("cut") {
			rule["cut"] = true
		}
	}
	body, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	return s.post(ctx, "/api/faults", body)
}
