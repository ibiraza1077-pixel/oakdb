package tests

import (
	"errors"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
)

// The classic bank test for isolation: writers move money between accounts
// inside transactions while readers keep summing every balance. If any
// reader ever sees a half-finished transfer, the total is wrong.
func TestConcurrentTransfersKeepTotalConstant(t *testing.T) {
	db, err := engine.Open(filepath.Join(t.TempDir(), "bank.oak"), engine.Options{NoSync: true, CheckpointFrames: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.NewSession()
	const accounts, start = 20, 1000
	if _, err := setup.Exec("CREATE TABLE acct (id INTEGER PRIMARY KEY, balance INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= accounts; i++ {
		setup.Exec("INSERT INTO acct VALUES ($1, $2)", record.NewInt(int64(i)), record.NewInt(start))
	}
	setup.Close()

	var transfers, retries, reads atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 16)

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			s := db.NewSession()
			defer s.Close()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 300; i++ {
				from, to := r.Intn(accounts)+1, r.Intn(accounts)+1
				amount := r.Intn(50)
				for {
					// Read, then write: this can hit a serialization failure
					// if another writer commits in between, so retry.
					_, err := s.Exec(`BEGIN;
						SELECT balance FROM acct WHERE id = $1;
						UPDATE acct SET balance = balance - $3 WHERE id = $1;
						UPDATE acct SET balance = balance + $3 WHERE id = $2;
						COMMIT`, record.NewInt(int64(from)), record.NewInt(int64(to)), record.NewInt(int64(amount)))
					if err == nil {
						transfers.Add(1)
						break
					}
					s.Exec("ROLLBACK")
					var e *engine.Error
					if errors.As(err, &e) && e.Code == engine.CodeSerialization {
						retries.Add(1)
						continue
					}
					errs <- err
					return
				}
			}
		}(int64(w))
	}
	var readers sync.WaitGroup
	for rd := 0; rd < 4; rd++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			s := db.NewSession()
			defer s.Close()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := s.Exec("SELECT sum(balance), count(*) FROM acct")
				if err != nil {
					errs <- err
					return
				}
				if sum := res[0].Rows[0][0].I; sum != accounts*start {
					errs <- errors.New("reader saw an inconsistent total: " + res[0].Rows[0][0].String())
					return
				}
				reads.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := db.Check(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d transfers (%d retried after serialization failures), %d consistent reads", transfers.Load(), retries.Load(), reads.Load())
}
