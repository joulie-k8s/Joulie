package main

import "flag"

// updateGoldens rewrites the goldens under testdata/ that the tests of this
// package compare against, instead of comparing:
//
//	go test ./simulator/cmd/simulator/ -run AnalyticCPUPower -update
//
// Review the diff before keeping it: a golden that moves is a change in
// simulated numbers.
var updateGoldens = flag.Bool("update", false, "rewrite the goldens under simulator/cmd/simulator/testdata/ from the current code")
