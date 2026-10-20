package transfers

import (
	"sync"
	"testing"
)

// Pieces of a parallel download that arrive at the same moment must show as
// one transfer.
func TestConcurrentJoinMakesOneTransfer(t *testing.T) {
	for round := 0; round < 200; round++ {
		tr := New()
		var wg sync.WaitGroup
		handles := make([]*Handle, 8)
		for i := range handles {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				handles[i] = tr.Join("dl", "guest", Download, "big.bin", 100, "p2p", nil)
			}(i)
		}
		wg.Wait()
		for _, h := range handles {
			if h != handles[0] {
				t.Fatalf("round %d: pieces got different transfers", round)
			}
		}
		if n := len(tr.Snapshot().Active); n != 1 {
			t.Fatalf("round %d: %d active transfers, want 1", round, n)
		}
	}
}
