package main

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/klauspost/compress/snappy"
)

// TestSnappyDecode checks snappyDecode (and the input length it reports
// consumed, which salvage scanning relies on) against a reference encoder -
// the Percona Server build validated against has no Snappy support, so no
// real SST file exercised it.
func TestSnappyDecode(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		n := r.Intn(200000)
		src := make([]byte, n)
		// mix of random and repetitive content
		for j := range src {
			if r.Intn(4) == 0 {
				src[j] = byte(r.Intn(256))
			} else {
				src[j] = byte('a' + j%7)
			}
		}
		enc := snappy.Encode(nil, src)
		out, used, err := snappyDecode(append(enc, 0xAA, 0xBB))
		if err != nil || !bytes.Equal(out, src) || used != len(enc) {
			t.Fatalf("case %d (n=%d): err=%v eq=%v used=%d/%d", i, n, err, bytes.Equal(out, src), used, len(enc))
		}
	}
}
