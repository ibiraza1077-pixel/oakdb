package tests

import (
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/pgwire"
)

// TestPythonPsycopg runs a real Python program using psycopg against an
// in-process OakDB server. It needs uv (https://docs.astral.sh/uv/) and is
// skipped when uv is not installed.
func TestPythonPsycopg(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv not installed")
	}
	db, err := engine.Open(filepath.Join(t.TempDir(), "py.oak"), engine.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &pgwire.Server{DB: db}
	go srv.Serve(ln)
	defer srv.Close()

	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "clients", "psycopg_check.py")
	url := fmt.Sprintf("postgresql://oak@%s/oakdb?sslmode=disable", ln.Addr())
	out, err := exec.Command(uv, "run", "--quiet", "--with", "psycopg[binary]", "python", script, url).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "psycopg ok") {
		t.Fatalf("python client failed: %v\n%s", err, out)
	}
}
