// Package web serves the operator interface, embedded in the binary.
//
// # DIRECTION CONTRACT
//
// THESIS: a logic analyzer for service traffic. Calls are events on per-service
// channels against a live time axis. It refuses the arrangement every debugging
// tool ships — a sortable table with a waterfall bolted on — because a table
// answers "what happened" and never "what was happening at the same time",
// which is the question when fifteen services are talking and one broke.
//
// OWN-WORLD: graphite instrument case, a recessed measurement field with a real
// grid, channel identity taken from the logic-probe colour code, monospace
// throughout, 1px hairlines, square corners, no glow and no shadow-as-
// separation.
//
// STORY: he opens it mid-debug, sees which channels are alive and which carry a
// fault, reads the failure without composing a query, selects it, and gets the
// decoded payload with its schema source named.
//
// FIRST VIEWPORT: measurement bar across the top with the fault filter already
// engaged; channel rail left; event field centre with lanes, fixed grid and a
// right-edge-is-now axis; inspector right.
//
// FORM: logic analyzer / timing diagram — candidate 3 of the grounded list,
// staged as bench instrument rather than phosphor CRT. Seed key 785490d1.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var assets embed.FS

// Handler serves the interface at the root of the API server.
func Handler() http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		// The files are compiled in; a failure here is a build problem, not a
		// runtime condition worth degrading for.
		panic(err)
	}

	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets are embedded and change only with the binary, so the browser
		// must not keep yesterday's build after an upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
