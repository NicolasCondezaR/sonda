package mcp

import (
	"context"
	"encoding/base64"
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
			Name:  "configure_service",
			Title: "Add or change a service",
			Description: "Add a service to a project, or change one that is already there. The name is the identity: call it with a name the project already has and that service is updated in place, so this is how you move a port Sonda could not take, correct an upstream, or switch a protocol. A name the project does not have is added. " +
				"Set tls when the caller refuses to speak http:// — Sonda then answers that port with a certificate it mints itself, and trust_certificate says what the user has to run to trust it.",
			Schema: obj(map[string]any{
				"project":  prop("string", "Project name."),
				"name":     prop("string", "Service name. An existing one is changed, a new one is added."),
				"listen":   prop("string", "Address Sonda should listen on, like 127.0.0.1:9152."),
				"upstream": prop("string", "Where the real service is, like http://localhost:50052, https://api.example.com, or postgres://localhost:5432 for a database. Never include a user or a password: Sonda forwards the client's own handshake and refuses an upstream that carries credentials."),
				"protocol": map[string]any{"type": "string", "enum": []string{"http", "grpc", "postgres"}, "description": "Defaults to http. Use postgres for a database: it gets a raw listener rather than an HTTP one."},
				"reflection": prop("boolean",
					"Ask this gRPC service for its own schema. Defaults to false, the same as connect_project, because a service that does not serve reflection is the ordinary case — give the project a descriptor set with upload_schemas instead. "+
						"Left out when changing a service, the current setting is kept."),
				"tls": prop("boolean", "Make Sonda terminate TLS on this port: the caller is pointed at https://listen instead of http://. Not available for postgres, which negotiates encryption inside its own protocol."),
				"insecure_skip_verify": prop("boolean",
					"Stop checking the upstream's certificate. Only for an https:// upstream, only for this one service, and never as a way to silence a certificate error you have not read. "+
						"Every capture taken through it is recorded as unverified and every interface shows the service as unverified, so this cannot be turned on quietly."),
			}, "project", "name", "listen", "upstream"),
			Annotations: writing,
			Run:         configureService,
		},

		{
			Name:  "remove_service",
			Title: "Remove a service",
			Description: "Delete a service from a project by name. The way out when a service cannot be fixed in place — the wrong protocol, a name that no longer matches anything — and the port closes with it if the project is listening. " +
				"Captures already taken are kept: they are evidence, and they outlive the configuration that produced them. Whatever was pointed at that port is now aimed at nothing, so the answer hands back the real address to put back.",
			Schema: obj(map[string]any{
				"project": prop("string", "Project name."),
				"name":    prop("string", "Service name, as list_services reports it."),
			}, "project", "name"),
			Annotations: changing,
			Run:         removeService,
		},

		{
			Name:  "upload_schemas",
			Title: "Give a project its gRPC schemas",
			Description: "Store a compiled protobuf descriptor set for a project, so gRPC messages come back with field names instead of numbered bytes. This is how a service that serves no reflection gets decoded, and that is the ordinary case: without it an agent can wire up a whole gRPC system and read nothing that crosses it. " +
				"Build one with `buf build -o descriptors.binpb` or `protoc --include_imports --descriptor_set_out=descriptors.binpb`, and pass the file base64-encoded — it is binary and cannot travel as text. " +
				"One per project: uploading again replaces what was there. list_services says whether a project already has one; schema_status says whether it is the thing actually being used.",
			Schema: obj(map[string]any{
				"project":        prop("string", "Project name."),
				"filename":       prop("string", "Name of the file, kept so every interface can say which descriptor set is loaded."),
				"content_base64": prop("string", "The descriptor set itself, base64-encoded."),
			}, "project", "filename", "content_base64"),
			// It stores configuration. Ports are left exactly as they are: the
			// schemas change how captured bytes are read, not what is listening.
			Annotations: writing,
			Run:         uploadSchemas,
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
				"A request with no recording gets a 501 saying so rather than an invented answer. Every stubbed answer carries an X-Sonda-Stub header, and stubbing is forgotten when Sonda restarts. " +
				"This is a switch on what is running, not configuration, so it only reaches services of the project whose ports are open: call activate_project first. list_services reports which services are stubbed right now.",
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
				"Faults are deterministic: one call in three means one call in three, the same sequence every run, so a failure can be reproduced. Every injected failure is recorded as injected and carries an X-Sonda-Fault header, and rules are forgotten when Sonda restarts. " +
				"Like stubbing, this is a switch on what is running, so it only reaches services of the project whose ports are open: call activate_project first. list_services reports which rules are in force right now.",
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
		// This is the only moment the real variable name is ever known, so it is
		// saved with the service. Empty for anything read out of a compose file,
		// which had no variable to begin with, and stored empty on purpose: the
		// gap is what disconnect_project reports instead of a derived name.
		service, err := json.Marshal(map[string]any{
			"name": f.Name, "listen": f.Listen, "upstream": f.Upstream, "protocol": f.Protocol,
			"env_key": f.Key,
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
				"problem": "that port is already in use, so it will not open — call configure_service with this same project and service name and a free listen address, which updates the service in place",
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
	project, err := findProject(ctx, s, a.str("project"))
	if err != nil {
		return nil, err
	}
	name := a.str("name")
	if name == "" {
		return nil, fmt.Errorf("the service needs a name")
	}

	protocol := a.str("protocol")
	if protocol == "" {
		protocol = "http"
	}
	service := map[string]any{
		"name": name, "listen": a.str("listen"),
		"upstream": a.str("upstream"), "protocol": protocol,
		"reflection": a.boolean("reflection"),
		"tls":        a.boolean("tls"), "insecure_skip_verify": a.boolean("insecure_skip_verify"),
	}

	// What separates a change from an addition is the id in the body — the same
	// field the web form fills in when it saves an existing row. Without it the
	// API inserts, the insert hits UNIQUE(project_id, name), and the tool that
	// promises to fix a port answers 400 for every service that exists.
	if existing := project.service(name); existing != nil {
		service["id"] = existing.ID
		if !a.has("reflection") {
			// Moving a port must not quietly turn schema resolution off for a
			// service that had it on.
			service["reflection"] = existing.Reflection
		}
	}

	body, err := json.Marshal(service)
	if err != nil {
		return nil, err
	}
	return s.post(ctx, fmt.Sprintf("/api/projects/%d/services", project.ID), body)
}

func removeService(ctx context.Context, s *Server, a args) (any, error) {
	project, err := findProject(ctx, s, a.str("project"))
	if err != nil {
		return nil, err
	}
	existing := project.service(a.str("name"))
	if existing == nil {
		return nil, fmt.Errorf("%q has no service called %q. It has: %s",
			project.Name, a.str("name"), strings.Join(project.serviceNames(), ", "))
	}

	if _, err := s.raw(ctx, "DELETE", fmt.Sprintf("/api/services/%d", existing.ID), nil); err != nil {
		return nil, err
	}
	return map[string]any{
		"removed": existing.Name,
		"project": project.Name,
		// Whatever was repointed at Sonda now names a port nobody holds, and
		// this is the address it has to go back to. Sonda cannot make that edit.
		"was_listening_on": existing.Listen,
		"point_back_at":    hostPortOf(existing.Upstream),
	}, nil
}

func uploadSchemas(ctx context.Context, s *Server, a args) (any, error) {
	id, err := projectID(ctx, s, a.str("project"))
	if err != nil {
		return nil, err
	}

	// Line breaks in a long base64 string are how it usually arrives, and
	// failing on them would look like a corrupt descriptor set.
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(a.str("content_base64")), ""))
	if err != nil {
		return nil, fmt.Errorf("content_base64 is not base64: %v", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("the descriptor set is empty — pass the bytes of a compiled descriptor set, base64-encoded")
	}

	filename := a.str("filename")
	if filename == "" {
		filename = "descriptors.binpb"
	}
	return s.post(ctx, fmt.Sprintf("/api/projects/%d/descriptor?name=%s", id, url.QueryEscape(filename)), raw)
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

	changes, byHand := reversePatch(before)
	next := "Put the changes above back into the configuration file and restart, so the callers talk to the real services again. Nothing was deleted: activate_project brings the project back exactly as it was."
	if len(byHand) > 0 {
		next += " The services under restore_by_hand are not in the patch because Sonda does not know which variable named them; each one says the address to search for and the address to put back."
	}

	out := map[string]any{
		"listening":  0,
		"changes":    changes,
		"next_steps": next,
	}
	if len(byHand) > 0 {
		out["restore_by_hand"] = byHand
	}
	return out, nil
}

// reversePatch rebuilds the edit that undoes connect_project, and reports what
// it cannot rebuild.
//
// A variable is only named when connect_project read that exact name out of the
// file and saved it with the service. Deriving one from the service and its
// protocol looks like it works and is wrong for every project that writes
// MS_AUTH_ADDR, MS_AUTH_HOST or MS_AUTH_HTTP_URL — all of which discovery
// accepts: the patch would set a variable nothing reads and leave the real one
// aimed at a port that just closed. That is the exact failure this tool exists
// to prevent, so a service whose variable is unknown is reported as the agent's
// to restore, with everything Sonda does know about it.
//
// The name comes back from the API with the service rather than from anything
// held here, so connecting in the morning and disconnecting in the evening is
// one workflow whether or not Sonda was restarted in between.
func reversePatch(projects any) (map[string]any, []map[string]string) {
	changes := map[string]any{}
	byHand := []map[string]string{}
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
			if name == "" {
				continue
			}
			// The scheme is dropped whatever it is, so a postgres:// target comes
			// back as the host:port the caller has to swap, not as a URL that
			// would be wrong to paste into a DSN.
			real := hostPortOf(upstream)

			variable, _ := service["env_key"].(string)
			if variable == "" {
				byHand = append(byHand, map[string]string{
					"service":          name,
					"was_listening_on": listen,
					"point_back_at":    real,
					"problem": "Sonda does not know which variable pointed at it: this service was added by hand, or came from a compose file, which has no variable to put back. " +
						"Find whatever in the configuration reads " + listen + " and set it back to " + real + ". Guessing the name here would write a variable nothing reads and leave the real one aimed at a closed port.",
				})
				continue
			}
			changes[variable] = map[string]string{"from": listen, "to": real}
		}
	}
	return changes, byHand
}

// hostPortOf strips whatever scheme an upstream was declared with.
func hostPortOf(upstream string) string {
	if _, after, found := strings.Cut(upstream, "://"); found {
		return after
	}
	return upstream
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

// knownProject is the part of a project's listing the configuration tools act
// on. The services come along because "is this service already there" is the
// question that decides between changing one and adding one, and asking for the
// listing twice to answer it would be two chances to see different states.
type knownProject struct {
	ID       int64          `json:"id"`
	Name     string         `json:"name"`
	Services []knownService `json:"services"`
}

type knownService struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Listen     string `json:"listen"`
	Upstream   string `json:"upstream"`
	Reflection bool   `json:"reflection"`
}

// service finds one by name the way a person would name it: the API is what
// enforces uniqueness, and it does so case-sensitively, but an agent that saw
// MS-Auth in a listing and typed ms-auth means the same service.
func (p knownProject) service(name string) *knownService {
	for i := range p.Services {
		if strings.EqualFold(p.Services[i].Name, name) {
			return &p.Services[i]
		}
	}
	return nil
}

func (p knownProject) serviceNames() []string {
	out := make([]string, 0, len(p.Services))
	for _, svc := range p.Services {
		out = append(out, svc.Name)
	}
	if len(out) == 0 {
		return []string{"nothing yet"}
	}
	return out
}

// findProject resolves a name, so every tool takes the name a person would say
// rather than a number they would have to look up.
func findProject(ctx context.Context, s *Server, name string) (knownProject, error) {
	if name == "" {
		return knownProject{}, fmt.Errorf("which project? pass its name")
	}
	payload, err := s.raw(ctx, "GET", "/api/projects", nil)
	if err != nil {
		return knownProject{}, err
	}
	var body struct {
		Projects []knownProject `json:"projects"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return knownProject{}, err
	}

	known := make([]string, 0, len(body.Projects))
	for _, p := range body.Projects {
		if strings.EqualFold(p.Name, name) {
			return p, nil
		}
		known = append(known, p.Name)
	}
	if len(known) == 0 {
		return knownProject{}, fmt.Errorf("there is no project called %q, and none exist yet — connect_project creates one", name)
	}
	return knownProject{}, fmt.Errorf("there is no project called %q. There is: %s", name, strings.Join(known, ", "))
}

func projectID(ctx context.Context, s *Server, name string) (int64, error) {
	p, err := findProject(ctx, s, name)
	return p.ID, err
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
	out, err := s.post(ctx, "/api/stub", body)
	return out, sequencing(err)
}

// sequencing turns "the active project has no service called X" into what it
// usually means. On its own that message reads like a typo, and an agent that
// believes it goes looking for the right spelling of a name that was right all
// along — when what is missing is the activation that would have put the
// service within reach.
func sequencing(err error) error {
	if err == nil || !strings.Contains(err.Error(), "active project has no service") {
		return err
	}
	return fmt.Errorf("%w. This reaches only the project whose ports are open, so if activate_project has not been called yet, that is the cause rather than the name; list_services says which project is active and what is in it", err)
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
	out, err := s.post(ctx, "/api/faults", body)
	return out, sequencing(err)
}
