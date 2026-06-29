package outputbuf

import "testing"

func TestRingReadOK(t *testing.T) {
	r := New(10)
	r.Write([]byte("hello"))
	res := r.Read(0, 0)
	if string(res.Output) != "hello" || res.NewCursor != 5 || res.SinceStatus != SinceOK {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestRingExpired(t *testing.T) {
	r := New(5)
	r.Write([]byte("abcdef"))
	res := r.Read(0, 0)
	if res.SinceStatus != SinceExpired {
		t.Fatalf("expected expired, got %+v", res)
	}
	if res.MissedBytes != 1 || string(res.Output) != "bcdef" {
		t.Fatalf("unexpected expired read: %+v output=%q", res, res.Output)
	}
	if res.TruncatedBy == nil || *res.TruncatedBy != TruncatedRingBuffer {
		t.Fatalf("expected ring_buffer truncation: %+v", res)
	}
}

func TestRingFuture(t *testing.T) {
	r := New(5)
	r.Write([]byte("abc"))
	res := r.Read(7, 0)
	if res.SinceStatus != SinceFuture || len(res.Output) != 0 || res.NewCursor != 7 {
		t.Fatalf("unexpected future read: %+v", res)
	}
}

func TestRingMaxBytes(t *testing.T) {
	r := New(20)
	r.Write([]byte("abcdef"))
	res := r.Read(0, 2)
	if string(res.Output) != "ab" || res.NewCursor != 2 {
		t.Fatalf("unexpected max read: %+v output=%q", res, res.Output)
	}
	if res.TruncatedBy == nil || *res.TruncatedBy != TruncatedMaxBytes {
		t.Fatalf("expected max_bytes truncation: %+v", res)
	}
}
