// Command oakdb runs the OakDB server or an interactive SQL shell.
//
//	oakdb serve [-data FILE] [-addr HOST:PORT] [-http HOST:PORT]
//	oakdb shell [-data FILE]
//	oakdb check [-data FILE]
//	oakdb version
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/pgwire"
)

//go:embed dashboard.html
var dashboardHTML []byte

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "shell":
		err = shell(os.Args[2:])
	case "check":
		err = checkCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(engine.VersionString)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "oakdb: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "oakdb:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `OakDB: a SQL database built from scratch.

Usage:
  oakdb serve [-data oak.db] [-addr 127.0.0.1:5433] [-http 127.0.0.1:8080]
        Start the server. Connect with any PostgreSQL client, e.g.
        psql "postgres://localhost:5433/oakdb?sslmode=disable"
  oakdb shell [-data oak.db]
        Open the database directly in an interactive SQL shell.
  oakdb check [-data oak.db]
        Verify every B-tree and index in the file.
  oakdb version
`)
}

func checkCmd(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	data := fs.String("data", "oak.db", "database file")
	fs.Parse(args)
	db, err := engine.Open(*data, engine.Options{})
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Check(); err != nil {
		return fmt.Errorf("integrity check failed: %w", err)
	}
	st := db.Stats()
	fmt.Printf("ok: %d pages, every table and index is consistent\n", st.PageCount)
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := fs.String("data", "oak.db", "database file")
	addr := fs.String("addr", "127.0.0.1:5433", "PostgreSQL protocol listen address")
	httpAddr := fs.String("http", "127.0.0.1:8080", "stats dashboard address (empty to disable)")
	noSync := fs.Bool("nosync", false, "skip fsync (faster, but a power cut can lose recent commits)")
	fs.Parse(args)

	db, err := engine.Open(*data, engine.Options{NoSync: *noSync})
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "oakdb ", log.LstdFlags)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		db.Close()
		return err
	}
	srv := &pgwire.Server{DB: db, Logger: logger}
	logger.Printf("database %s, PostgreSQL protocol on %s", *data, ln.Addr())

	var web *http.Server
	if *httpAddr != "" {
		web = &http.Server{Addr: *httpAddr, Handler: dashboard(db), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := web.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("dashboard: %v", err)
			}
		}()
		logger.Printf("dashboard on http://%s", *httpAddr)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Printf("shutting down")
		srv.Close()
		if web != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			web.Shutdown(ctx)
		}
	}()
	if err := srv.Serve(ln); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}

func dashboard(db *engine.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})
	mux.HandleFunc("/stats.json", func(w http.ResponseWriter, r *http.Request) {
		st := db.Stats()
		hitRate := 0.0
		if total := st.CacheHits + st.CacheMisses; total > 0 {
			hitRate = float64(st.CacheHits) / float64(total)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"version":        engine.VersionString,
			"uptime_seconds": int(st.Uptime.Seconds()),
			"queries":        st.Queries,
			"errors":         st.Errors,
			"sessions":       st.ActiveSessions,
			"commits":        st.Commits,
			"rollbacks":      st.Rollbacks,
			"checkpoints":    st.Checkpoints,
			"wal_frames":     st.WALFrames,
			"pages":          st.PageCount,
			"cached_pages":   st.CachedPages,
			"cache_hits":     st.CacheHits,
			"cache_misses":   st.CacheMisses,
			"cache_hit_rate": hitRate,
			"active_readers": st.ActiveReads,
			"database_bytes": int64(st.PageCount) * 4096,
		})
	})
	mux.HandleFunc("/schema.json", func(w http.ResponseWriter, r *http.Request) {
		s, err := db.Schema()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})
	return mux
}
