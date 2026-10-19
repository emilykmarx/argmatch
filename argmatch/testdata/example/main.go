package main

import (
	"fmt"
	"io"
	"net"
)

type ID string
type ID2 string
type ID3 string
type ID4 string
type ID5 string

type Hier map[string]int
type Named struct{ Name ID3 }
type Inner struct{ Name ID4 }
type Outer struct {
	P *Inner
}
type Iface interface{ M() }

func (i ID5) M() {}

// Where the match is => output
// Named basic type => name
// Basic key of named map => map
// Basic elem of non-named array => nothing
// Named field of named struct => field and struct
// Named field of nested struct via pointer => field and both structs
// Receiver
// Interface
func (id_recvr ID) F(id_arg ID2, h Hier, arr []string,
	in Named, o *Outer,
	i Iface) {
	// Send a message
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	go func() {
		peer, err := ln.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		io.Copy(io.Discard, peer)
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	conn.Write([]byte(id_recvr))
}

type Handler struct{ Path ID }

func (h *Handler) Apply() error { return nil }

type Reloader struct {
	Name string
	Fn   func() error
}

// Matches through closures => the closure and what it captured
// Method value in a named struct
// Closure capturing a named struct
// Closure capturing a basic string
func G(r Reloader, named func(), basic func()) {}

func main() {
	needle := "needle"
	id := ID(needle)
	in := Inner{Name: ID4(needle)}
	named := Named{Name: ID3(needle)}
	out := Outer{P: &in}
	hier := Hier{needle: 5}
	id.F(ID2(needle), hier, []string{needle}, named, &out, ID5(needle))
	h := &Handler{Path: ID(needle)}
	G(Reloader{Name: "r", Fn: h.Apply}, func() { _ = named }, func() { _ = needle })
	fmt.Println("done")
}
