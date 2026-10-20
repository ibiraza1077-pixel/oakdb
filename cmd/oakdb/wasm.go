//go:build js && wasm

// The browser build of OakDB. It is the same engine, pager, WAL and B+tree as
// the native binary; the page provides an in-memory file system, so oak.db and
// oak.db-wal live in browser memory instead of on disk.
//
//	GOOS=js GOARCH=wasm go build -o docs/demo/oakdb.wasm ./cmd/oakdb
//
// It exposes a global `oakdb` object to JavaScript:
//
//	oakdb.exec(text)  runs SQL or a backslash command; returns the shell output
//	oakdb.status()    JSON with engine counters and the transaction state
//	oakdb.crash()     abandons the open database without closing it, as if the
//	                  process were killed, then reopens it, which runs WAL recovery
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"syscall/js"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
)

const browserDBPath = "/oak.db"

type browserShell struct {
	sh  *shellState
	out bytes.Buffer
}

func main() {
	b := &browserShell{}
	api := js.Global().Get("Object").New()
	api.Set("version", engine.VersionString)
	api.Set("exec", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 {
			return ""
		}
		return b.exec(args[0].String())
	}))
	api.Set("status", js.FuncOf(func(js.Value, []js.Value) any { return b.status() }))
	api.Set("crash", js.FuncOf(func(js.Value, []js.Value) any { return b.crash() }))
	if err := b.open(); err != nil {
		api.Set("error", err.Error())
	}
	js.Global().Set("oakdb", api)
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("oakdb-ready"))
	select {} // keep the Go runtime alive for callbacks
}

func (b *browserShell) open() error {
	db, err := engine.Open(browserDBPath, engine.Options{CachePages: 1024})
	if err != nil {
		return err
	}
	b.sh = &shellState{db: db, sess: db.NewSession(), out: &b.out, timing: true}
	return nil
}

// exec runs one input: a backslash command or one or more SQL statements.
func (b *browserShell) exec(q string) (out string) {
	b.out.Reset()
	defer func() {
		// A panic must not take down the Go runtime, or every later call fails.
		if r := recover(); r != nil {
			out = b.out.String() + fmt.Sprintf("internal error: %v\n", r)
		}
	}()
	if b.sh == nil {
		return "ERROR: database is not open\n"
	}
	t := strings.TrimSpace(q)
	switch {
	case t == "":
	case strings.HasPrefix(t, `\`):
		if b.sh.meta(t) {
			fmt.Fprintln(&b.out, "Nowhere to quit to: this database lives in your browser tab.")
		}
	default:
		b.sh.run(q)
	}
	return b.out.String()
}

func (b *browserShell) status() string {
	if b.sh == nil {
		return "{}"
	}
	st := b.sh.db.Stats()
	j, _ := json.Marshal(map[string]any{
		"pages": st.PageCount, "cachedPages": st.CachedPages,
		"cacheHits": st.CacheHits, "cacheMisses": st.CacheMisses,
		"commits": st.Commits, "rollbacks": st.Rollbacks,
		"checkpoints": st.Checkpoints, "walFrames": st.WALFrames,
		"queries": st.Queries, "errors": st.Errors,
		"tx": string(b.sh.sess.TxStatus()),
	})
	return string(j)
}

// crash drops the open database on the floor: no Close, no checkpoint, no
// rollback, exactly what a killed process leaves behind. Reopening the files
// runs the same WAL recovery the SIGKILL tests exercise.
func (b *browserShell) crash() string {
	b.sh = nil
	if err := b.open(); err != nil {
		return "ERROR: " + err.Error()
	}
	return "ok"
}
