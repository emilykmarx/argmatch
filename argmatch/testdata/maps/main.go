package main

import (
	"fmt"
	"strconv"
)

type Name string
type Big struct {
	Pad  [200]byte
	Name Name
}
type BigKey struct {
	Pad [150]byte
	ID  int
}
type SKey struct{ A, B int }
type Holder struct {
	Small  map[string]Name
	Large  map[int]Name
	Strs   map[Name]bool
	BigVal map[string]Big
	BigKey map[BigKey]Name
	SK     map[SKey]Name
	Nested map[string]map[uint16]Name
	Any    any
	Bools  map[bool]*Big
}

func (h *Holder) F() {}

func main() {
	h := &Holder{
		Small:  map[string]Name{"a": "needle", "b": "x"},
		Large:  map[int]Name{},
		Strs:   map[Name]bool{"needle": true, "other": false},
		BigVal: map[string]Big{"k": {Name: "needle"}},
		BigKey: map[BigKey]Name{{ID: 7}: "needle"},
		SK:     map[SKey]Name{{1, -2}: "needle"},
		Nested: map[string]map[uint16]Name{"outer": {65535: "needle"}},
		Any:    map[int8]Name{-3: "needle"},
		Bools:  map[bool]*Big{true: {Name: "needle"}},
	}
	for i := range 40 {
		h.Large[i*1000] = Name("v" + strconv.Itoa(i))
	}
	h.Large[-5] = "needle"
	h.F()
	fmt.Println("done")
}
