package tests

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

// Crash testing: a child process writes transactions as fast as it can and
// reports each one the moment COMMIT returns. The parent kills it with
// SIGKILL at a random moment (mid-write, mid-fsync, mid-checkpoint, ...),
// reopens the database and checks two promises:
//
//  1. Durability: every transaction the child reported as committed is there.
//  2. Atomicity: every transaction is either fully there or not at all.
//
// It repeats this many times on the same file, so recovery also has to cope
// with a database that has already been through earlier crashes.

const (
	crashChildEnv = "OAKDB_CRASH_CHILD"
	rowsPerTxn    = 4
)

func crashChild(path string) {
	db, err := engine.Open(path, engine.Options{CheckpointFrames: 40, CachePages: 64})
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	s := db.NewSession()
	if _, err := s.Exec(`CREATE TABLE IF NOT EXISTS log (id INTEGER PRIMARY KEY, batch INT, payload TEXT);
		CREATE INDEX IF NOT EXISTS log_batch ON log (batch)`); err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	res, err := s.Exec("SELECT coalesce(max(batch), 0) FROM log")
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	batch := res[0].Rows[0][0].I
	out := bufio.NewWriter(os.Stdout)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		batch++
		if _, err := s.Exec("BEGIN"); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		for i := 0; i < rowsPerTxn; i++ {
			// Mix of small rows and rows big enough to need overflow pages.
			payload := strings.Repeat(string(rune('a'+r.Intn(26))), r.Intn(6000))
			if _, err := s.Exec("INSERT INTO log (batch, payload) VALUES ($1, $2)", record.NewInt(batch), record.NewText(payload)); err != nil {
				fmt.Println("ERR", err)
				os.Exit(1)
			}
		}
		// Occasionally delete an old batch, so pages are freed and reused.
		if batch%5 == 0 {
			if _, err := s.Exec("DELETE FROM log WHERE batch = $1", record.NewInt(batch-3)); err != nil {
				fmt.Println("ERR", err)
				os.Exit(1)
			}
		}
		if _, err := s.Exec("COMMIT"); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "COMMITTED %d\n", batch)
		out.Flush()
	}
}

func TestCrashRecovery(t *testing.T) {
	if path := os.Getenv(crashChildEnv); path != "" {
		crashChild(path)
		return
	}
	rounds := 25
	if s := os.Getenv("OAKDB_CRASH_ROUNDS"); s != "" {
		rounds, _ = strconv.Atoi(s)
	}
	if testing.Short() {
		rounds = 5
	}
	path := filepath.Join(t.TempDir(), "crash.oak")
	r := rand.New(rand.NewSource(42))
	var lastAcked int64
	deleted := map[int64]bool{}
	for round := 0; round < rounds; round++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery$")
		cmd.Env = append(os.Environ(), crashChildEnv+"="+path)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		killAt := time.Duration(30+r.Intn(400)) * time.Millisecond
		timer := time.AfterFunc(killAt, func() { cmd.Process.Signal(syscall.SIGKILL) })
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "ERR") {
				timer.Stop()
				cmd.Process.Kill()
				t.Fatalf("child failed: %s", line)
			}
			if n, ok := strings.CutPrefix(line, "COMMITTED "); ok {
				b, _ := strconv.ParseInt(n, 10, 64)
				lastAcked = b
				if b%5 == 0 {
					deleted[b-3] = true
				}
			}
		}
		cmd.Wait()
		timer.Stop()

		db, err := engine.Open(path, engine.Options{})
		if err != nil {
			t.Fatalf("round %d: reopen after crash: %v", round, err)
		}
		if err := db.Check(); err != nil {
			t.Fatalf("round %d: integrity check after crash: %v", round, err)
		}
		s := db.NewSession()
		res, err := s.Exec("SELECT batch, count(*) FROM log GROUP BY batch")
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		counts := map[int64]int64{}
		var maxBatch int64
		for _, row := range res[0].Rows {
			counts[row[0].I] = row[1].I
			maxBatch = max(maxBatch, row[0].I)
		}
		// Atomicity: no partial batches.
		for b, n := range counts {
			if n != rowsPerTxn {
				t.Fatalf("round %d: batch %d has %d rows, want %d (a transaction was partly applied)", round, b, n, rowsPerTxn)
			}
		}
		// At most one unacknowledged commit (in flight when killed) may exist.
		if maxBatch > lastAcked+1 {
			t.Fatalf("round %d: found batch %d but only %d were acknowledged", round, maxBatch, lastAcked)
		}
		if maxBatch == lastAcked+1 {
			// It committed but the child died before reporting it; adopt it.
			lastAcked = maxBatch
			if maxBatch%5 == 0 {
				deleted[maxBatch-3] = true
			}
		}
		// Durability: every acknowledged batch (that was not deleted) exists.
		for b := int64(1); b <= lastAcked; b++ {
			if _, ok := counts[b]; !ok && !deleted[b] {
				t.Fatalf("round %d: acknowledged batch %d is missing after the crash", round, b)
			}
		}
		s.Close()
		db.Close()
		t.Logf("round %d: killed after %v, %d batches durable", round, killAt, lastAcked)
	}
	if lastAcked == 0 {
		t.Fatal("the child never committed anything; the test proved nothing")
	}
}
