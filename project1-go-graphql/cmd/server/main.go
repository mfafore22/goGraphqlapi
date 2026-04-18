// Command taskflow is the HTTP server for the Go + GraphQL task manager.
//
// Architecture overview (full version in README.md):
//
//   HTTP request
//       -> chi router
//          -> auth middleware (extracts JWT, populates context)
//             -> /graphql        -- graphql-go handler -> resolvers -> store -> pgx -> Postgres
//             -> /api/events     -- server-sent events stream wired to TaskPubSub
//             -> /               -- static files from web/ (the HTML frontend)
//
// Why this layout:
//   - cmd/server owns ONLY wiring. Zero business logic here.
//   - Every dependency is constructed once at startup and injected down.
//   - The process exits cleanly on SIGINT/SIGTERM so containers reap properly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/graphql-go/graphql"

	"taskflow/internal/auth"
	"taskflow/internal/db"
	"taskflow/internal/gql"
	"taskflow/internal/store"
)

func main() {
	// Fail fast on missing config. We could default JWT_SECRET in dev, but
	// then someone would deploy to prod without setting it. Loud errors > silent defaults.
	if os.Getenv("JWT_SECRET") == "" {
		log.Fatal("JWT_SECRET must be set")
	}

	// Root context cancelled on SIGINT/SIGTERM. Everything downstream
	// (the server, the DB pool, in-flight requests) watches this for shutdown.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ---- Dependencies ----
	pool, err := db.Connect(ctx)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	st := store.New(pool)
	pubsub := gql.NewTaskPubSub()
	resolver := gql.NewResolver(st, pubsub)

	schema, err := gql.New(resolver)
	if err != nil {
		log.Fatalf("build schema: %v", err)
	}

	// ---- Router ----
	r := chi.NewRouter()
	r.Use(middleware.RequestID)                        // correlation id per request
	r.Use(middleware.RealIP)                           // honor X-Forwarded-For behind proxies
	r.Use(middleware.Logger)                           // one-line access log
	r.Use(middleware.Recoverer)                        // panic -> 500, don't crash
	r.Use(middleware.Timeout(30 * time.Second))        // hard cap on a single request
	r.Use(corsMiddleware)                              // permissive for demo (see below)
	r.Use(auth.Middleware)                             // parse JWT, populate context

	// GraphQL endpoint.
	r.Post("/graphql", graphqlHandler(schema.Schema))

	// Server-sent events (SSE) for live task updates. SSE is simpler than
	// WebSockets for one-way server-to-client streams: it uses plain HTTP,
	// reconnects automatically in the browser, and works through proxies
	// that would block WebSockets. Tradeoff: one-way only; if we needed
	// client-to-server streaming we'd switch to WebSockets.
	r.Get("/api/events", sseHandler(pubsub, st))

	// Static frontend. http.FileServer handles If-Modified-Since etc.
	r.Handle("/*", http.FileServer(http.Dir("web")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second, // defend against slowloris
	}

	// Run the server in a goroutine so we can watch for ctx cancellation
	// in main and initiate graceful shutdown.
	go func() {
		log.Printf("taskflow listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done() // block until SIGINT/SIGTERM
	log.Println("shutting down...")

	// Give in-flight requests 10s to finish. After that, force-close.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// graphqlHandler turns an http.Request into a graphql.Do call.
// Inlining this instead of using graphql-go/handler gives us full control
// over the request/response shape and clearer error reporting.
func graphqlHandler(schema graphql.Schema) http.HandlerFunc {
	type reqBody struct {
		Query         string                 `json:"query"`
		OperationName string                 `json:"operationName"`
		Variables     map[string]interface{} `json:"variables"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body reqBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		// graphql.Do runs parse -> validate -> execute. Context threads
		// through so resolvers can access the authenticated user ID.
		result := graphql.Do(graphql.Params{
			Schema:         schema,
			RequestString:  body.Query,
			VariableValues: body.Variables,
			OperationName:  body.OperationName,
			Context:        r.Context(),
		})
		// We always return 200 for GraphQL, with errors inside the body.
		// This matches the GraphQL-over-HTTP spec and what clients like
		// Apollo/urql expect.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

// sseHandler streams task updates for a project to a connected client.
// Protocol is plain Server-Sent Events: `data: <json>\n\n` per message.
//
// Auth: token comes via query string because EventSource in the browser does
// NOT let you set custom headers. In production you'd prefer a short-lived
// "SSE ticket" token minted from a real auth'd request, rather than passing
// the real JWT on the URL (where it can leak into server logs). See README.
func sseHandler(ps *gql.TaskPubSub, st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse auth from the query string specifically (the middleware
		// only checks the Authorization header).
		uid := auth.UserFromContext(r.Context())
		if uid == "" {
			if tok := r.URL.Query().Get("token"); tok != "" {
				if id, err := auth.ParseToken(tok); err == nil {
					uid = id
				}
			}
		}
		if uid == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		projectID := r.URL.Query().Get("projectId")
		if projectID == "" {
			http.Error(w, "projectId required", http.StatusBadRequest)
			return
		}
		// Verify the caller owns the project before subscribing.
		proj, err := st.ProjectByID(r.Context(), projectID)
		if err != nil || proj.OwnerID != uid {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Required SSE headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Flusher lets us push each event immediately instead of waiting
		// for the response buffer to fill. If the server is behind a proxy
		// that buffers responses (e.g., some nginx configs), SSE won't work
		// until X-Accel-Buffering: no is set.
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch, cleanup := ps.Subscribe(projectID)
		defer cleanup()

		// Heartbeat ticker keeps idle connections alive through proxies
		// and lets the browser detect a dead server quickly.
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return // client disconnected
			case t, ok := <-ch:
				if !ok {
					return // pubsub cleaned up
				}
				b, _ := json.Marshal(t)
				fmt.Fprintf(w, "event: task\ndata: %s\n\n", b)
				flusher.Flush()
			case <-ticker.C:
				// Comment lines are ignored by EventSource but keep the
				// TCP connection warm.
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

// corsMiddleware is permissive to make the demo trivial to run. A real app
// would either reflect only an allow-listed origin or use a mature package
// like github.com/rs/cors.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
