// Command catalog is the bookshop service that owns the database.
//
// It is the only service here that runs SQL, which makes it the one that shows
// Sonda's Postgres capture doing the thing it exists for: a statement appearing
// as a child of the HTTP request that ran it.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/NicolasCondezaR/sonda/testbed/internal/toy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var flags = toy.NewFlags("drift")

var (
	db *sql.DB

	// Kept so the login handler can open a connection of its own from the same
	// settings without re-reading the environment.
	connConfig *pgx.ConnConfig
)

func main() {
	addr := flag.String("addr", ":8101", "address to listen on")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		slog.Error("DATABASE_URL is required; point it at Sonda's postgres port")
		os.Exit(1)
	}

	var err error
	connConfig, err = pgx.ParseConfig(dsn)
	if err != nil {
		slog.Error("parse DATABASE_URL", "error", err)
		os.Exit(1)
	}

	// The driver pings a connection that has been idle for a second before
	// reusing it. A ping is a statement, so it turns up in the field as an
	// extra child of whichever request happened to pick that connection up —
	// nothing to do with Sonda, and confusing in exactly the exercise the tree
	// is for. A real service should leave this alone.
	noPing := stdlib.OptionShouldPing(func(context.Context, stdlib.ShouldPingParams) bool { return false })
	db = stdlib.OpenDB(*connConfig, noPing)
	defer db.Close()

	mux := http.NewServeMux()
	flags.Handle(mux)

	// No database call: a health check that queried would put a statement in
	// the field every few seconds and bury the traffic worth reading.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		toy.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /books/{sku}", getBook)
	mux.HandleFunc("GET /books", searchBooks)
	mux.HandleFunc("GET /reviews", getReviews)
	mux.HandleFunc("POST /login", login)
	mux.HandleFunc("GET /slow", slow)

	toy.Listen(*addr, mux)
}

// getBook is the endpoint the contract-drift and diff exercises are built on.
//
// The DSN asks pgx for one Parse/Bind/Describe/Execute/Sync cycle per query, so
// the capture carries the SQL and the value bound to $1 together.
func getBook(w http.ResponseWriter, r *http.Request) {
	sku := r.PathValue("sku")

	// An ordinary HTTP 500, selected by the request rather than by a switch, so
	// there is always one flowing beside the calls that work.
	if sku == "KAPUT" {
		toy.Fail(w, http.StatusInternalServerError, "catalog index is corrupt for this SKU")
		return
	}

	const q = `SELECT sku, title, author, price_cents, discount_pct, in_stock
	           FROM books WHERE sku = $1`

	var (
		title, author           string
		priceCents, discountPct int
		inStock                 bool
	)
	err := db.QueryRowContext(r.Context(), q, sku).
		Scan(&sku, &title, &author, &priceCents, &discountPct, &inStock)
	if errors.Is(err, sql.ErrNoRows) {
		toy.Fail(w, http.StatusNotFound, "no such SKU: "+sku)
		return
	}
	if err != nil {
		toy.Fail(w, http.StatusBadGateway, "catalog query failed: "+err.Error())
		return
	}

	book := map[string]any{
		"sku":          sku,
		"title":        title,
		"author":       author,
		"price_cents":  priceCents,
		"discount_pct": discountPct,
		"in_stock":     inStock,
	}

	// The drift switch. Three changes at once, because they are the three the
	// report has to tell apart: a field gone, a field retyped, and a field
	// added — of which only the first two would break a caller.
	if flags.On("drift") {
		delete(book, "discount_pct")
		book["price_cents"] = strconv.Itoa(priceCents)
		book["cached"] = false
	}

	toy.JSON(w, http.StatusOK, book)
}

func searchBooks(w http.ResponseWriter, r *http.Request) {
	term := r.URL.Query().Get("q")
	if term == "" {
		term = "e"
	}

	rows, err := db.QueryContext(r.Context(),
		`SELECT sku, title FROM books WHERE title ILIKE $1 ORDER BY sku`, "%"+term+"%")
	if err != nil {
		toy.Fail(w, http.StatusBadGateway, "search failed: "+err.Error())
		return
	}
	defer rows.Close()

	out := []map[string]string{}
	for rows.Next() {
		var sku, title string
		if err := rows.Scan(&sku, &title); err != nil {
			toy.Fail(w, http.StatusBadGateway, "search failed: "+err.Error())
			return
		}
		out = append(out, map[string]string{"sku": sku, "title": title})
	}
	if err := rows.Err(); err != nil {
		toy.Fail(w, http.StatusBadGateway, "search failed: "+err.Error())
		return
	}
	toy.JSON(w, http.StatusOK, map[string]any{"query": term, "books": out})
}

// getReviews answers normally, and with ?broken=1 runs a statement the server
// rejects. A SQL error has no status code anywhere — it arrives as an
// ErrorResponse inside the stream — so it is the failure a tool that only reads
// transports would show as healthy.
func getReviews(w http.ResponseWriter, r *http.Request) {
	sku := r.URL.Query().Get("sku")
	if sku == "" {
		sku = "DUNE"
	}

	q := `SELECT stars, body FROM reviews WHERE sku = $1 ORDER BY id`
	if r.URL.Query().Get("broken") == "1" {
		q = `SELECT stars, body FROM reviews_archive_2019 WHERE sku = $1 ORDER BY id`
	}

	rows, err := db.QueryContext(r.Context(), q, sku)
	if err != nil {
		toy.Fail(w, http.StatusBadGateway, "reviews query failed: "+err.Error())
		return
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var stars int
		var body string
		if err := rows.Scan(&stars, &body); err != nil {
			toy.Fail(w, http.StatusBadGateway, "reviews scan failed: "+err.Error())
			return
		}
		out = append(out, map[string]any{"stars": stars, "body": body})
	}
	if err := rows.Err(); err != nil {
		toy.Fail(w, http.StatusBadGateway, "reviews query failed: "+err.Error())
		return
	}
	toy.JSON(w, http.StatusOK, map[string]any{"sku": sku, "reviews": out})
}

// login carries a credential into three of the places redaction has to reach:
// a JSON request body, an Authorization header the caller sent, and — because
// this query is deliberately built with the password inline — a SQL literal.
func login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		toy.Fail(w, http.StatusBadRequest, "body must be {\"email\":…,\"password\":…}")
		return
	}

	// A connection of its own rather than one out of the pool, so the Postgres
	// startup handshake — and the database password blanked inside it — is in
	// every login capture instead of only in the first statement the process
	// ever ran. Nothing else here would show that blanking happening.
	conn, err := pgx.ConnectConfig(r.Context(), connConfig.Copy())
	if err != nil {
		toy.Fail(w, http.StatusBadGateway, "login connection failed: "+err.Error())
		return
	}
	defer conn.Close(context.Background())

	// QueryExecModeSimpleProtocol puts the values into the SQL text, so the
	// member's password crosses the wire as a literal inside the statement.
	// That is what Sonda blanks when a statement names a credential, and a
	// bound parameter would not show it.
	var name string
	err = conn.QueryRow(r.Context(),
		`SELECT name FROM members WHERE email = $1 AND password = $2`,
		pgx.QueryExecModeSimpleProtocol, in.Email, in.Password).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		toy.Fail(w, http.StatusUnauthorized, "no member with that email and password")
		return
	}
	if err != nil {
		toy.Fail(w, http.StatusBadGateway, "login query failed: "+err.Error())
		return
	}

	toy.JSON(w, http.StatusOK, map[string]any{
		"name":          name,
		"session_token": "sess-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
}

// slow succeeds after a delay. Latency is a different problem from failure and
// the field draws it differently — a wide mark, not a fault bar.
func slow(w http.ResponseWriter, r *http.Request) {
	ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
	if ms <= 0 {
		ms = 800
	}
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
	case <-r.Context().Done():
		return
	}
	toy.JSON(w, http.StatusOK, map[string]any{"slept_ms": ms, "report": "shelf audit complete"})
}
