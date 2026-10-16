// Package model holds the data structures shared by every part of PairToPairSync:
// version vectors, file metadata and block hashing.
package model

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Vector is a version vector: one monotonically increasing counter per device
// (keyed by the short device ID). It lets two devices tell whether one version
// of a file supersedes another or whether both were edited independently.
type Vector map[string]uint64

// Ordering is the result of comparing two vectors.
type Ordering int

const (
	Equal Ordering = iota
	Greater
	Lesser
	Concurrent
)

// Compare returns how v relates to o.
func (v Vector) Compare(o Vector) Ordering {
	vGreater, oGreater := false, false
	for k, a := range v {
		if a > o[k] {
			vGreater = true
		}
	}
	for k, b := range o {
		if b > v[k] {
			oGreater = true
		}
	}
	switch {
	case vGreater && oGreater:
		return Concurrent
	case vGreater:
		return Greater
	case oGreater:
		return Lesser
	default:
		return Equal
	}
}

// Dominates reports whether v is equal to or newer than o.
func (v Vector) Dominates(o Vector) bool {
	c := v.Compare(o)
	return c == Equal || c == Greater
}

// Clone returns an independent copy.
func (v Vector) Clone() Vector {
	n := make(Vector, len(v)+1)
	for k, x := range v {
		n[k] = x
	}
	return n
}

// Bump returns a copy of v in which the counter of device id is advanced. The
// counter never goes below the wall clock, so a device that lost its database
// still produces versions that supersede the ones it created before.
func (v Vector) Bump(id string) Vector {
	n := v.Clone()
	c := n[id] + 1
	if now := uint64(time.Now().Unix()); now > c {
		c = now
	}
	n[id] = c
	return n
}

// Merge returns the element-wise maximum of v and o.
func (v Vector) Merge(o Vector) Vector {
	n := v.Clone()
	for k, x := range o {
		if x > n[k] {
			n[k] = x
		}
	}
	return n
}

// String is a canonical representation, usable as a deterministic tie-breaker.
func (v Vector) String() string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(v[k], 10))
	}
	return b.String()
}
