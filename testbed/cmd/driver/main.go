// Command driver is the shop's customers: it drives the same eighteen steps
// round and round so the field has live traffic in it without anyone pasting
// curl commands.
//
// The mix is deliberate and fixed. Roughly two thirds of it works and one third
// fails, the failures are of different kinds, and which is which is decided by
// the request rather than by chance — run it twice and the same steps fail. A
// generator that rolled dice would make every exercise in the guide say
// "probably".
//
// Steps run one at a time, with a pause between them. Sonda arranges calls into
// a tree by containment, so two overlapping requests would produce trees that
// are honestly reported as ambiguous and hard to read.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	shopv1 "github.com/NicolasCondezaR/sonda/testbed/gen/shop/v1"
	"github.com/NicolasCondezaR/sonda/testbed/internal/ws"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	gatewayURL    = env("GATEWAY_URL", "http://127.0.0.1:9401")
	catalogURL    = env("CATALOG_URL", "http://127.0.0.1:9402")
	storefrontURL = env("STOREFRONT_URL", "http://127.0.0.1:9403")
	pricingAddr   = env("PRICING_ADDR", "127.0.0.1:9404")

	client = &http.Client{Timeout: 30 * time.Second}
)

func main() {
	once := flag.Bool("once", false, "run one cycle and stop")
	period := flag.Duration("period", 20*time.Second, "wait this long between cycles")
	flag.Parse()

	for n := 1; ; n++ {
		log.Printf("── cycle %d ──", n)
		cycle()
		if *once {
			return
		}
		time.Sleep(*period)
	}
}

func cycle() {
	// Works: a whole checkout, every branch green.
	step("checkout DUNE", checkout(map[string]any{
		"sku": "DUNE", "quantity": 1, "customer": "cust-1",
	}))
	step("checkout PALE-FIRE x5", checkout(map[string]any{
		"sku": "PALE-FIRE", "quantity": 5, "customer": "cust-2",
	}))

	// Fails, each in a different way, each as one branch of a checkout that
	// got further than the branch that broke.
	step("checkout RESTRICTED-1 (gRPC PermissionDenied under HTTP 200)", checkout(map[string]any{
		"sku": "RESTRICTED-1", "quantity": 1, "customer": "cust-1",
	}))
	step("checkout SOLARIS for a customer that does not exist (GraphQL errors under HTTP 200)", checkout(map[string]any{
		"sku": "SOLARIS", "quantity": 1, "customer": "ghost",
	}))
	step("checkout DUNE express (shipping is not running)", checkout(map[string]any{
		"sku": "DUNE", "quantity": 1, "customer": "cust-1", "express": true,
	}))
	step("checkout KAPUT (an ordinary HTTP 500 from the catalog)", checkout(map[string]any{
		"sku": "KAPUT", "quantity": 1, "customer": "cust-1",
	}))

	// Slow but fine, then slow enough that the gateway stops waiting.
	step("report 400ms (slow, succeeds)", get(gatewayURL+"/report?ms=400"))
	step("report 2500ms (the gateway gives up at 1s)", get(gatewayURL+"/report?ms=2500"))

	// SQL, working and broken.
	step("reviews for DUNE", get(catalogURL+"/reviews?sku=DUNE"))
	step("reviews from a table that does not exist (SQL error, no status code)", get(catalogURL+"/reviews?sku=DUNE&broken=1"))
	step("search the catalog", get(catalogURL+"/books?q=a"))

	// Credentials, in the three places redaction has to reach.
	step("log in (password in a JSON body and in a SQL literal)", login())
	step("oauth callback (credentials in the query string)",
		get(gatewayURL+"/oauth/callback?code=ac-9f21&access_token=tok-live-2f8c11&state=shelf"))

	// Sockets and streams.
	step("websocket, closed cleanly", socket("shelf?"))
	step("websocket, closed with 1011 and a reason", socket("boom"))
	step("server-sent events", get(storefrontURL+"/events"))

	// gRPC streaming, straight at the price service.
	step("gRPC server stream", watch())

	// A GraphQL batch: several operations in one POST.
	step("graphql batch", postJSON(storefrontURL+"/graphql", []any{
		map[string]any{"query": "query Shelf { shelf }"},
		map[string]any{
			"query":     "query CustomerOrders($id: ID!) { customer(id: $id) { id name } }",
			"variables": map[string]any{"id": "cust-3"},
		},
	}))
}

func checkout(body map[string]any) error {
	return postJSON(gatewayURL+"/checkout", body)
}

func login() error {
	return postJSON(catalogURL+"/login", map[string]any{
		"email":    "ada@bookshop.test",
		"password": "shelf-of-books",
	})
}

// socket holds one short conversation and reads the close frame, so the whole
// exchange is over and Sonda writes the capture. A socket is captured when it
// closes, not while it is open.
func socket(say string) error {
	conn, err := ws.Dial(strings.Replace(storefrontURL, "https://", "http://", 1) + "/ws")
	if err != nil {
		return err
	}
	if _, _, err := conn.Read(); err != nil { // the welcome frame
		return err
	}
	if err := conn.Text(say); err != nil {
		return err
	}
	for {
		op, payload, err := conn.Read()
		if err != nil {
			conn.Hangup()
			return nil
		}
		if op == ws.OpClose {
			code, reason := ws.CloseCode(payload)
			conn.Hangup()
			return fmt.Errorf("closed %d: %s", code, reason)
		}
		if strings.Contains(string(payload), "SOLARIS") {
			// The last message of a good conversation: say goodbye and let the
			// server answer with its own close frame.
			return conn.CloseWith(ws.CloseNormal, "done reading")
		}
	}
}

func watch() error {
	conn, err := grpc.NewClient(pricingAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := shopv1.NewPricingClient(conn).WatchQuote(ctx, &shopv1.QuoteRequest{
		Sku: "SOLARIS", Quantity: 2, Currency: "USD",
	})
	if err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func get(url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return send(req)
}

func postJSON(url string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return send(req)
}

// send adds the credential every customer of this shop carries, so the header
// is on real traffic rather than only on the one call an exercise makes.
func send(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer tok-live-2f8c11")
	req.Header.Set("User-Agent", "bookshop-driver/1")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, oneLine(body))
	}
	return nil
}

// step logs what happened. `docker compose logs driver` is then a readable
// script of what should be in the field, failures included and expected.
func step(what string, err error) {
	if err != nil {
		log.Printf("   %-70s FAILED — %v", what, err)
	} else {
		log.Printf("   %-70s ok", what)
	}
	// Long enough that two steps never overlap, short enough that a cycle is
	// over in seconds.
	time.Sleep(150 * time.Millisecond)
}

func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
