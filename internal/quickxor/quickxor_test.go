package quickxor

import (
	"crypto/rand"
	"testing"
)

func TestEmpty(t *testing.T) {
	if got := Base64(New()); got != "AAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Fatalf("empty = %s", got)
	}
}

func TestChunkingInvariance(t *testing.T) {
	buf := make([]byte, 100_003)
	_, _ = rand.Read(buf)
	whole := New()
	whole.Write(buf)
	want := Base64(whole)
	for _, step := range []int{1, 7, 19, 20, 21, 4096, 65537} {
		h := New()
		for i := 0; i < len(buf); i += step {
			end := min(i+step, len(buf))
			h.Write(buf[i:end])
		}
		if got := Base64(h); got != want {
			t.Fatalf("step %d: %s != %s", step, got, want)
		}
	}
}

func TestLengthMatters(t *testing.T) {
	a, b := New(), New()
	a.Write([]byte{0})
	b.Write([]byte{0, 0})
	if Base64(a) == Base64(b) {
		t.Fatal("length not mixed into hash")
	}
}
