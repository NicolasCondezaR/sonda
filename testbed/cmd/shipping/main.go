// Command shipping quotes delivery for an express order.
//
// It is not started by `docker compose up`. That is deliberate: a service that
// is simply not running is one of the failure kinds worth seeing, and it looks
// nothing like a service that answered badly. Bring it up with
// `docker compose --profile shipping up -d shipping` and the same call starts
// working without anything else changing.
package main

import (
	"flag"
	"net/http"

	"github.com/NicolasCondezaR/sonda/testbed/internal/toy"
)

func main() {
	addr := flag.String("addr", ":8104", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		toy.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /quote", func(w http.ResponseWriter, _ *http.Request) {
		toy.JSON(w, http.StatusOK, map[string]any{
			"carrier":    "Bookmobile",
			"cost_cents": 590,
			"eta_days":   2,
		})
	})

	toy.Listen(*addr, mux)
}
