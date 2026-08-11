// Command gateway is the front door of the bookshop, and the only service that
// calls other services.
//
// Every call it makes goes to a Sonda port rather than to the service itself,
// which is what puts a whole request in the field as one tree instead of as
// four unrelated rows. Fan-out is sequential on purpose: Sonda arranges calls
// into a tree by containment, and two children running at the same time make
// the nesting genuinely ambiguous — which Sonda would then honestly report as
// ambiguous, and which would make every tree in the guide harder to read.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	shopv1 "github.com/NicolasCondezaR/sonda/testbed/gen/shop/v1"
	"github.com/NicolasCondezaR/sonda/testbed/internal/toy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

var (
	catalogURL    = env("CATALOG_URL", "http://127.0.0.1:9402")
	storefrontURL = env("STOREFRONT_URL", "http://127.0.0.1:9403")
	shippingURL   = env("SHIPPING_URL", "http://127.0.0.1:9405")
	pricingAddr   = env("PRICING_ADDR", "127.0.0.1:9404")

	pricing shopv1.PricingClient
	client  = &http.Client{Timeout: 30 * time.Second}
)

func main() {
	addr := flag.String("addr", ":8100", "address to listen on")
	flag.Parse()

	// Lazy: nothing is dialled until the first call, so the gateway starts
	// whether or not Sonda's port is open yet.
	conn, err := grpc.NewClient(pricingAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("pricing client", "error", err)
		os.Exit(1)
	}
	defer conn.Close()
	pricing = shopv1.NewPricingClient(conn)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		toy.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /checkout", checkout)
	mux.HandleFunc("GET /report", report)
	mux.HandleFunc("GET /oauth/callback", oauthCallback)

	toy.Listen(*addr, mux)
}

type checkoutRequest struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Customer string `json:"customer"`
	Express  bool   `json:"express"`
}

// checkout is the fan-out: catalog, then pricing over gRPC, then the storefront
// over GraphQL, then shipping when the order is express.
//
// Which of those fail is decided by the request — a restricted SKU, a customer
// that does not exist, an express order while shipping is down — so one run
// produces trees that are entirely green and trees with exactly one red branch,
// at the same time, without anything being switched.
func checkout(w http.ResponseWriter, r *http.Request) {
	var in checkoutRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		toy.Fail(w, http.StatusBadRequest, "body must be {\"sku\":…,\"quantity\":…,\"customer\":…}")
		return
	}
	if in.Quantity <= 0 {
		in.Quantity = 1
	}

	order := map[string]any{"sku": in.SKU, "quantity": in.Quantity}

	book, err := getJSON(r, catalogURL+"/books/"+in.SKU)
	if err != nil {
		gatewayFailed(w, "catalog", err)
		return
	}
	order["book"] = book

	quote, err := getQuote(r, in)
	if err != nil {
		gatewayFailed(w, "pricing", err)
		return
	}
	order["quote"] = quote

	customer, err := getCustomer(r, in.Customer)
	if err != nil {
		gatewayFailed(w, "storefront", err)
		return
	}
	order["customer"] = customer

	if in.Express {
		shipping, err := postJSON(r, shippingURL+"/quote", map[string]any{"sku": in.SKU})
		if err != nil {
			gatewayFailed(w, "shipping", err)
			return
		}
		order["shipping"] = shipping
	}

	toy.JSON(w, http.StatusOK, order)
}

// report calls a slow endpoint under a deadline the gateway sets itself.
//
// ?ms= above that deadline is a timeout rather than a failure: nothing answered
// badly, the caller stopped waiting. Sonda records it as a call with an error
// and no status, which is a different-looking row from a 500.
func report(w http.ResponseWriter, r *http.Request) {
	ms := r.URL.Query().Get("ms")
	if ms == "" {
		ms = "800"
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL+"/slow?ms="+ms, nil)
	toy.Propagate(r.Header, req.Header)

	resp, err := client.Do(req)
	if err != nil {
		toy.Fail(w, http.StatusGatewayTimeout, "the catalog did not answer within 1s: "+err.Error())
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		slog.Error("write report", "error", err)
	}
}

// oauthCallback is where a real system puts a credential in a URL. It exists so
// that a captured path carries one, which is the third place redaction has to
// reach after headers and bodies.
func oauthCallback(w http.ResponseWriter, r *http.Request) {
	toy.JSON(w, http.StatusOK, map[string]any{
		"linked": true,
		"code":   r.URL.Query().Get("code") != "",
	})
}

func getQuote(r *http.Request, in checkoutRequest) (any, error) {
	ctx := r.Context()
	// gRPC metadata is HTTP/2 headers, so the same names Sonda reads on an
	// HTTP call group a gRPC call into the same request.
	for _, h := range toy.TraceHeaders {
		if v := r.Header.Get(h); v != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, h, v)
		}
	}

	resp, err := pricing.Quote(ctx, &shopv1.QuoteRequest{
		Sku:      in.SKU,
		Quantity: int32(in.Quantity),
		Currency: "USD",
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"unit_cents":  resp.GetUnitCents(),
		"total_cents": resp.GetTotalCents(),
		"currency":    resp.GetCurrency(),
		"tier":        resp.GetTier().String(),
	}, nil
}

func getCustomer(r *http.Request, id string) (any, error) {
	if id == "" {
		id = "cust-1"
	}
	body := map[string]any{
		"query":         "query CustomerOrders($id: ID!) { customer(id: $id) { id name tier orders { id total_cents } } }",
		"operationName": "CustomerOrders",
		"variables":     map[string]any{"id": id},
	}

	raw, err := postJSON(r, storefrontURL+"/graphql", body)
	if err != nil {
		return nil, err
	}

	// A GraphQL error arrives under HTTP 200, so a caller that only checked the
	// status code would carry on with a nil customer. This one looks.
	if obj, ok := raw.(map[string]any); ok {
		if errs, ok := obj["errors"].([]any); ok && len(errs) > 0 {
			return nil, fmt.Errorf("GraphQL returned %d error(s) under HTTP 200", len(errs))
		}
	}
	return raw, nil
}

func getJSON(r *http.Request, url string) (any, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	toy.Propagate(r.Header, req.Header)
	// The caller's credential is passed on, like any gateway does. It ends up
	// in the capture of every hop, which is what MCP redaction has to catch.
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return send(req)
}

func postJSON(r *http.Request, url string, body any) (any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	toy.Propagate(r.Header, req.Header)
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return send(req)
}

func send(req *http.Request) (any, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s answered %d: %s", req.URL.Path, resp.StatusCode, trim(body))
	}

	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s answered %d with something that is not JSON", req.URL.Path, resp.StatusCode)
	}
	return out, nil
}

// gatewayFailed names the hop that broke. The step is in the body so a reader
// who found this call first knows which branch of the tree to open.
func gatewayFailed(w http.ResponseWriter, step string, err error) {
	toy.JSON(w, http.StatusBadGateway, map[string]any{
		"error":  "checkout failed at the " + step + " step",
		"step":   step,
		"detail": err.Error(),
		"status": http.StatusBadGateway,
	})
}

func trim(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
