// The functions here take channels of named types, for TestChans: argmatch
// lists each chosen function's channel parameters, with the named types in
// their element types.
package main

import (
	"fmt"
	"time"

	tgp "example.com/chans/tg"
)

type Local struct{ Name string }

type R struct{ Name string }

// Run has a receive-only and a send-only channel.
func (r R) Run(in <-chan map[string][]*tgp.Group, out chan<- Local) {
	for m := range in {
		for k := range m {
			out <- Local{Name: k}
		}
	}
	close(out)
}

// Send has no receiver, so -recvonly doesn't instrument it, but its channel
// is still listed.
func Send(ch chan []tgp.Group, n int) {
	for i := 0; i < n; i++ {
		ch <- []tgp.Group{{Source: "needle"}}
	}
	close(ch)
}

// Plain's channels' elements aren't named types of the program: a predeclared
// type, and an imported one.
func Plain(a chan int, d chan time.Duration) {}

// Gen's channel's element is a type parameter.
func Gen[T any](ch chan T) {}

func main() {
	in, out := make(chan map[string][]*tgp.Group, 1), make(chan Local, 1)
	in <- map[string][]*tgp.Group{"needle": nil}
	close(in)
	R{Name: "needle"}.Run(in, out)
	<-out
	ch := make(chan []tgp.Group, 1)
	Send(ch, 1)
	Plain(nil, nil)
	Gen[int](nil)
	fmt.Println("done")
}
