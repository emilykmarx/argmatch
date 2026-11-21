// A program for TestCarriers: alerts, identified by their labels, handed to
// Put and Notify, and held deeper in Store.
package main

import (
	"context"
	"fmt"
	"runtime/pprof"
)

type Labels map[string]string

// Model is embedded in Alert, as Alertmanager's types.Alert embeds
// model.Alert, whose Labels the carrier spec names.
type Model struct {
	Labels Labels
	Note   string
}

type Alert struct {
	Model
	N int
}

type Store struct {
	held []*Alert // two levels into the receiver: past -carrierdepth 1
}

// Put is handed alerts as a variadic parameter.
func Put(alerts ...*Alert) {}

// Notify is a method, handed alerts as a parameter.
func (s *Store) Notify(n int, alerts ...*Alert) {}

func main() {
	// The same labels, built in different orders, so that the maps' memory
	// order may differ; and an alert with other labels.
	a := &Alert{Model: Model{Labels: Labels{"alertname": "A", "severity": "page", "z": "1"}, Note: "x"}, N: 1}
	b := &Alert{Model: Model{Labels: Labels{"z": "1", "severity": "page", "alertname": "A"}, Note: "y"}, N: 2}
	c := &Alert{Model: Model{Labels: Labels{"alertname": "C"}}}
	// Put runs while serving a request: the labels the ConfTamer patch to
	// net/http sets
	pprof.Do(context.Background(), pprof.Labels("conftamer.request", "id:7", "conftamer.request_goroutine", "1"),
		func(context.Context) { Put(a, c) })
	s := &Store{held: []*Alert{c}}
	s.Notify(3, b)
	fmt.Println("done")
}
