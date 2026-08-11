package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	// The one package outside the API these tools reach for. Two services given
	// the same suggested port is a clash neither side can see — see FreeListen —
	// and the answer has to be the same one discovery itself gives, or the
	// preview and the thing that gets created disagree.
	"github.com/NicolasCondezaR/sonda/internal/discover"
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
				"It returns the exact variable changes to apply so the traffic starts flowing through Sonda: that part is yours, because Sonda cannot repoint a caller. It does not activate the project; call activate_project when you are ready for the ports to open. " +
				"Safe to run again with the same name: an existing project is added to, a service it already has is updated in place with whatever the file says today, and settings the file cannot express — TLS, certificate checking — are kept.",
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
			Description: "Add a service to a project, or change one that is already there. The name is the identity: call it with a name the project already has and that service is updated in place, so this is how you move a port Sonda could not take, correct an upstream, or switch a protocol. A name the project does not have is added, and then listen and upstream are both required. " +
				"An update keeps every setting you do not pass, so moving a port is just the project, the name and the new listen. It answers with the address to point the caller at, because Sonda cannot repoint one. " +
				"Set tls when the caller refuses to speak http:// — Sonda then answers that port with a certificate it mints itself, and trust_certificate says what the user has to run to trust it.",
			Schema: obj(map[string]any{
				"project":  prop("string", "Project name."),
				"name":     prop("string", "Service name. An existing one is changed, a new one is added."),
				"listen":   prop("string", "Address Sonda should listen on, like 127.0.0.1:9152. Required when adding; left out when changing, the current one is kept."),
				"upstream": prop("string", "Where the real service is, like http://localhost:50052, https://api.example.com, or postgres://localhost:5432 for a database. Never include a user or a password: Sonda forwards the client's own handshake and refuses an upstream that carries credentials. Required when adding; left out when changing, the current one is kept."),
				"protocol": map[string]any{"type": "string", "enum": []string{"http", "grpc", "postgres"}, "description": "Defaults to http when adding. Use postgres for a database: it gets a raw listener rather than an HTTP one. Left out when changing a service, the current one is kept."},
				"reflection": prop("boolean",
					"Ask this gRPC service for its own schema. Defaults to false when adding, the same as connect_project, because a service that does not serve reflection is the ordinary case — give the project a descriptor set with upload_schemas instead. "+
						"Left out when changing a service, the current setting is kept."),
				"tls": prop("boolean", "Make Sonda terminate TLS on this port: the caller is pointed at https://listen instead of http://. Not available for postgres, which negotiates encryption inside its own protocol. Left out when changing a service, the current setting is kept."),
				"insecure_skip_verify": prop("boolean",
					"Stop checking the upstream's certificate. Only for an https:// upstream, only for this one service, and never as a way to silence a certificate error you have not read. "+
						"Every capture taken through it is recorded as unverified and every interface shows the service as unverified, so this cannot be turned on quietly."),
			}, "project", "name"),
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
				"One per project: uploading again replaces what was there. list_services says whether a project already has one; schema_status says whether it is the thing actually being used. " +
				"Over about three megabytes the encoded upload does not fit in one message on the stdio transport and is refused; point `sonda mcp` at a running Sonda over HTTP for a descriptor set that large.",
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

// discovered is one service as /api/discover reports it.
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

func connectProject(ctx context.Context, s *Server, a args) (any, error) {
	name := a.str("name")
	if name == "" {
		return nil, fmt.Errorf("the project needs a name")
	}

	rawFiles, _ := a["files"].([]any)
	if len(rawFiles) == 0 {
		return nil, fmt.Errorf("pass at least one configuration file, with its contents")
	}

	found, order, err := readFiles(ctx, s, rawFiles)
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("no services found in those files — Sonda looks for entries like MS_AUTH_GRPC_URL=localhost:50052, or published ports in a compose file")
	}

	project, fresh, err := openProject(ctx, s, name)
	if err != nil {
		return nil, err
	}

	changes := map[string]any{}
	conflicts := []map[string]string{}
	added, updated := 0, 0

	for _, key := range order {
		f := found[key]
		existing := project.service(f.Name)
		service := serviceFor(f, existing)

		body, err := json.Marshal(service)
		if err != nil {
			return nil, err
		}
		if _, err := s.raw(ctx, "POST", fmt.Sprintf("/api/projects/%d/services", project.ID), body); err != nil {
			// One bad entry must not lose the other twenty. It is reported
			// beside the service it belongs to instead.
			conflicts = append(conflicts, map[string]string{"service": f.Name, "problem": err.Error()})
			continue
		}
		if existing != nil {
			updated++
		} else {
			added++
		}

		listen, _ := service["listen"].(string)
		if f.PortTaken {
			conflicts = append(conflicts, map[string]string{
				"service": f.Name,
				"listen":  listen,
				"problem": "that port is already in use, so it will not open — call configure_service with this same project and service name and a free listen address, which updates the service in place",
			})
		}
		// An entry whose file already names Sonda's own port is not a change to
		// make: the edit is there from the last run.
		if f.Key != "" && f.Original != listen {
			changes[f.Key] = map[string]string{"from": f.Original, "to": listen}
		}
	}

	if added+updated == 0 && fresh {
		// Nothing was saved and the project only exists because this call made
		// it. Left behind it is an empty row the agent has no tool to remove.
		if _, err := s.raw(ctx, "DELETE", fmt.Sprintf("/api/projects/%d", project.ID), nil); err != nil {
			return nil, fmt.Errorf("no service could be saved, and the empty project could not be removed either (%v); the first problem was: %s",
				err, conflicts[0]["problem"])
		}
		return nil, fmt.Errorf("no service could be saved, so the empty project was removed again and nothing was left behind. The first problem was: %s",
			conflicts[0]["problem"])
	}

	out := map[string]any{
		"project":    project.Name,
		"services":   added + updated,
		"added":      added,
		"updated":    updated,
		"active":     false,
		"changes":    changes,
		"next_steps": "Apply the changes above to the file they came from, call activate_project, then restart whatever makes these calls so it picks up the new addresses. Sonda cannot repoint a caller by itself.",
	}
	if !fresh {
		out["reused_existing_project"] = fmt.Sprintf(
			"%q was already there, so this was added to it rather than refused: %d services were updated in place and %d were new. Anything else in the project was left alone.",
			project.Name, updated, added)
	}
	if len(conflicts) > 0 {
		out["problems"] = conflicts
	}
	return out, nil
}

// readFiles turns the files the agent pasted into the set of services to create.
//
// A service named in two of them is one service, and the first reading wins —
// re-adding it would only fail on the unique constraint further down and turn a
// duplicate into an error.
func readFiles(ctx context.Context, s *Server, rawFiles []any) (map[string]discovered, []string, error) {
	found := map[string]discovered{}
	order := []string{}

	// Each file is read on its own, so each one only avoids collisions with
	// itself. A project is usually several, and two suggestions that land on
	// one address produce two services where only one can ever bind — with the
	// loser reported as a port somebody else is holding. Carrying the set
	// across the whole run is what closes that.
	taken := map[string]bool{}

	for _, entry := range rawFiles {
		file, _ := entry.(map[string]any)
		filename, _ := file["filename"].(string)
		content, _ := file["content"].(string)
		if content == "" {
			continue
		}

		payload, err := s.raw(ctx, "POST", "/api/discover?filename="+url.QueryEscape(filename), []byte(content))
		if err != nil {
			return nil, nil, err
		}
		var body struct {
			Found []discovered `json:"found"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, nil, err
		}
		for _, f := range body.Found {
			if _, seen := found[f.Name]; seen {
				continue
			}
			f.Listen = discover.FreeListen(f.Listen, taken)
			found[f.Name] = f
			order = append(order, f.Name)
		}
	}
	return found, order, nil
}

// openProject returns the project to write into, and whether this call is what
// created it.
//
// A name that is already taken used to end the run with a bare 409 and no way
// forward: there is no delete_project and no rename_project, so an agent that
// fixed its .env and asked again was simply stuck. Re-running is the ordinary
// case rather than a mistake — it is what connect_project's own answer tells
// the agent to do after editing the file — so an existing project is added to.
// Which project the agent means is not in doubt: it passed the name.
func openProject(ctx context.Context, s *Server, name string) (knownProject, bool, error) {
	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		return knownProject{}, false, err
	}
	status, payload, err := s.api.Call(ctx, http.MethodPost, "/api/projects", body)
	if err != nil {
		return knownProject{}, false, err
	}

	switch {
	case status < 400:
		var project knownProject
		if err := json.Unmarshal(payload, &project); err != nil {
			return knownProject{}, false, err
		}
		return project, true, nil

	case status == http.StatusConflict:
		project, err := findProject(ctx, s, name)
		return project, false, err

	default:
		return knownProject{}, false, fmt.Errorf("Sonda answered %d: %s", status, apiError(payload))
	}
}

// serviceFor is the body that saves one discovered service.
//
// A name the project already has is updated in place — the id is what makes the
// API update rather than insert — and everything a configuration file cannot
// say is carried over from the row that is there. A .env knows nothing about
// TLS, and connecting a project twice must not answer a port in plaintext that
// every caller now reaches over https://.
func serviceFor(f discovered, existing *knownService) map[string]any {
	service := map[string]any{
		"name": f.Name, "listen": f.Listen, "upstream": f.Upstream, "protocol": f.Protocol,
		// This is the only moment the real variable name is ever known, so it is
		// saved with the service. Empty for anything read out of a compose file,
		// which had no variable to begin with, and stored empty on purpose: the
		// gap is what disconnect_project reports instead of a derived name.
		"env_key": f.Key,
	}
	if existing == nil {
		return service
	}

	service["id"] = existing.ID
	// The stored spelling wins: renaming ms-auth to MS-Auth would orphan every
	// capture already taken under the old one.
	service["name"] = existing.Name
	service["reflection"] = existing.Reflection
	service["tls"] = existing.TLS
	service["insecure_skip_verify"] = existing.InsecureSkipVerify

	// The second run reads the file the first run told the agent to edit, so
	// the address in it is Sonda's own port. Saving that as the upstream would
	// point the service at itself; the real one is the one already stored.
	if hostPortOf(f.Upstream) == existing.Listen {
		service["listen"] = existing.Listen
		service["upstream"] = existing.Upstream
	}
	return service
}

// configureService adds a service, or changes the one that is already there.
//
// An update starts from the stored row and applies only what the caller
// actually passed. Guarding field by field was the shape that lost TLS: whoever
// moves a port names listen and nothing else, and every field nobody remembered
// to guard went back to its zero value — so the listener came back in plaintext
// while every caller still said https://, and the tool reported success. The
// next field added would have been the next one silently dropped.
func configureService(ctx context.Context, s *Server, a args) (any, error) {
	project, err := findProject(ctx, s, a.str("project"))
	if err != nil {
		return nil, err
	}
	name := a.str("name")
	if name == "" {
		return nil, fmt.Errorf("the service needs a name")
	}

	existing := project.service(name)
	svc := knownService{Name: name, Protocol: "http"}
	if existing != nil {
		// The id is what makes the API update rather than insert. Without it
		// the insert hits UNIQUE(project_id, name), and the tool that promises
		// to fix a port answers 400 for every service that exists.
		svc = *existing
	} else if a.str("listen") == "" || a.str("upstream") == "" {
		return nil, fmt.Errorf("%q has no service called %q, so this would add one, and a new service needs both listen and upstream. It has: %s",
			project.Name, name, strings.Join(project.serviceNames(), ", "))
	}

	// Only what arrived. Everything else is whatever the row already said.
	if a.has("listen") {
		svc.Listen = a.str("listen")
	}
	if a.has("upstream") {
		svc.Upstream = a.str("upstream")
	}
	if a.has("protocol") {
		svc.Protocol = a.str("protocol")
	}
	if a.has("reflection") {
		svc.Reflection = a.boolean("reflection")
	}
	if a.has("tls") {
		svc.TLS = a.boolean("tls")
	}
	if a.has("insecure_skip_verify") {
		svc.InsecureSkipVerify = a.boolean("insecure_skip_verify")
	}

	body, err := json.Marshal(svc)
	if err != nil {
		return nil, err
	}
	if _, err := s.post(ctx, fmt.Sprintf("/api/projects/%d/services", project.ID), body); err != nil {
		return nil, err
	}
	return configuredService(project, svc, existing, name), nil
}

// configuredService is the answer: the address to point at, and the edit that
// does it.
//
// The API replies with the whole project listing, and the one thing that was
// asked — where this service answers now — is buried in it. Sonda cannot
// repoint a caller, so the tool whose job is fixing a port owes the agent the
// same explicit patch connect_project and disconnect_project hand back.
func configuredService(project knownProject, svc knownService, existing *knownService, requested string) map[string]any {
	address := svc.Listen
	if svc.TLS {
		// A TLS listener answers nothing on http://, so the address handed over
		// has to carry the scheme or it is one that will not work.
		address = "https://" + svc.Listen
	}

	out := map[string]any{
		"project":      project.Name,
		"service":      svc.Name,
		"listening_on": address,
		"protocol":     svc.Protocol,
		"upstream":     svc.Upstream,
		"tls":          svc.TLS,
		"next_steps":   "Point whatever calls this service at the address above and restart it. Nothing reaches Sonda until that edit is made, and Sonda cannot make it.",
	}
	if svc.InsecureSkipVerify {
		out["insecure_skip_verify"] = true
	}

	switch {
	case existing == nil:
		out["added"] = true
	case existing.Listen != svc.Listen:
		out["moved_from"] = existing.Listen
	}
	if svc.EnvKey != "" && existing != nil && existing.Listen != svc.Listen {
		// The variable this service was really pointed by, as discovery read it
		// out of the file — the same one disconnect_project puts back.
		out["changes"] = map[string]any{
			svc.EnvKey: map[string]string{"from": existing.Listen, "to": address},
		}
	} else if svc.EnvKey == "" {
		out["which_variable"] = "Sonda does not know which variable names this service, so search the configuration for " + svc.Listen + " and set whatever holds it to the address above."
	}

	// Names match case-insensitively, because an agent that read MS-Auth in a
	// listing and typed ms-auth means the same service. Writing the new
	// spelling through would rename the row and orphan every capture already
	// taken under the old one, so the stored name stays and the difference is
	// said out loud instead of applied quietly.
	if requested != svc.Name {
		out["note"] = fmt.Sprintf("%q is stored as %q and keeps that spelling: renaming it would orphan every capture already taken under it, so search_calls still wants %q",
			requested, svc.Name, svc.Name)
	}
	return out
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

// knownService carries every field a service is stored with, and not only the
// ones a tool happens to read today. It is what an update is built from, so a
// field missing here is a field an update silently resets — which is how moving
// a port used to turn TLS back off.
//
// The id is what tells the API to update rather than insert, and it is left out
// when there is none so an addition is not read as an update of row zero.
type knownService struct {
	ID                 int64  `json:"id,omitempty"`
	Name               string `json:"name"`
	Listen             string `json:"listen"`
	Upstream           string `json:"upstream"`
	Protocol           string `json:"protocol"`
	Reflection         bool   `json:"reflection"`
	TLS                bool   `json:"tls"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`

	// EnvKey is never sent to change it — an empty one leaves the stored value
	// alone — but it is read, because it names the variable a moved port has to
	// be written into.
	EnvKey string `json:"env_key,omitempty"`
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
