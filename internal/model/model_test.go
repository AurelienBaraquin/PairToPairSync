package model

import (
	"bytes"
	"testing"
)

func TestVectorCompare(t *testing.T) {
	a := Vector{"A": 2, "B": 1}
	cases := []struct {
		o    Vector
		want Ordering
	}{
		{Vector{"A": 2, "B": 1}, Equal},
		{Vector{"A": 1, "B": 1}, Greater},
		{Vector{"A": 3, "B": 1}, Lesser},
		{Vector{"A": 1, "B": 2}, Concurrent},
		{Vector{}, Greater},
		{Vector{"C": 1}, Concurrent},
	}
	for _, c := range cases {
		if got := a.Compare(c.o); got != c.want {
			t.Errorf("%v vs %v: got %v want %v", a, c.o, got, c.want)
		}
	}
}

func TestBumpSupersedesAndMerge(t *testing.T) {
	v := Vector{"A": 5}
	n := v.Bump("A")
	if n.Compare(v) != Greater || v["A"] != 5 {
		t.Fatalf("bump must produce a newer, independent vector: %v %v", v, n)
	}
	x, y := Vector{"A": 3, "B": 1}, Vector{"A": 1, "B": 4}
	m := x.Merge(y)
	if !m.Dominates(x) || !m.Dominates(y) {
		t.Fatalf("merge must dominate both: %v", m)
	}
}

// Both devices must pick the same winner whatever the argument order.
func TestWinnerIsDeterministic(t *testing.T) {
	a := FileInfo{ModTime: 100, Version: Vector{"A": 1}}
	b := FileInfo{ModTime: 200, Version: Vector{"B": 1}}
	if a.WinsOver(b) == b.WinsOver(a) {
		t.Fatal("exactly one side must win")
	}
	c := FileInfo{ModTime: 100, Version: Vector{"C": 1}}
	if a.WinsOver(c) == c.WinsOver(a) {
		t.Fatal("tie-break must be antisymmetric")
	}
	del := FileInfo{ModTime: 999, Deleted: true, Version: Vector{"D": 1}}
	if del.WinsOver(a) || !a.WinsOver(del) {
		t.Fatal("a modification must beat a deletion")
	}
}

func TestHashBlocks(t *testing.T) {
	data := bytes.Repeat([]byte("x"), minBlockSize*2+10)
	bs, blocks, n, err := HashBlocks(bytes.NewReader(data), int64(len(data)))
	if err != nil || n != int64(len(data)) || bs != minBlockSize || len(blocks) != 3 {
		t.Fatalf("bs=%d blocks=%d n=%d err=%v", bs, len(blocks), n, err)
	}
	fi := FileInfo{Size: n, BlockSize: bs}
	if off, sz := fi.BlockRange(2); off != int64(2*minBlockSize) || sz != 10 {
		t.Fatalf("last block range wrong: %d %d", off, sz)
	}
	if _, blocks, n, _ := HashBlocks(bytes.NewReader(nil), 0); len(blocks) != 0 || n != 0 {
		t.Fatal("empty file has no blocks")
	}
	if BlockSizeFor(1<<40) != maxBlockSize {
		t.Fatal("block size must be capped")
	}
}
