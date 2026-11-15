package database_test

import (
	"sync"
	"testing"

	"toft/internal/database/dbtest"
)

// **A burst keeps the connections it opened.** With five idle allowed under
// twenty-five open, every connection past the fifth was closed the moment it
// was returned and dialled again by the next burst — over TLS and SCRAM against
// RDS, which is what filled trytoft's pool at 5% database CPU (drills,
// 2026-10-28). The pool may grow to its cap; what it opened stays idle until
// ConnMaxIdleTime retires it.
func TestABurstKeepsTheConnectionsItOpened(t *testing.T) {
	db := dbtest.New(t)
	pool := db.Conn().SQL()

	const burst = 20
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.Conn().Exec(`SELECT pg_sleep(0.1)`); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	st := pool.Stats()
	if st.MaxIdleClosed != 0 {
		t.Errorf("the pool closed %d connection(s) for exceeding its idle cap; a burst must keep them", st.MaxIdleClosed)
	}
	if st.Idle < burst {
		t.Errorf("idle = %d after a burst of %d; the connections it opened were not kept", st.Idle, burst)
	}
}
