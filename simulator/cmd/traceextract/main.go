package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	var inPath string
	var outPath string
	flag.StringVar(&inPath, "in", "", "input JSONL file")
	flag.StringVar(&outPath, "out", "trace.jsonl", "output trace JSONL file")
	flag.Parse()
	if inPath == "" {
		panic("-in is required")
	}

	in, err := os.Open(inPath)
	if err != nil {
		panic(err)
	}
	defer in.Close()
	out, err := os.Create(outPath)
	if err != nil {
		panic(err)
	}
	defer out.Close()

	s := bufio.NewScanner(in)
	w := bufio.NewWriter(out)
	n := 0
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue
		}
		if _, ok := obj["type"]; !ok {
			obj["type"] = "job"
		}
		if _, ok := obj["schemaVersion"]; !ok {
			obj["schemaVersion"] = "v1"
		}
		b, err := json.Marshal(obj)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: marshal failed, skipping record: %v\n", err)
			continue
		}
		_, _ = w.Write(append(b, '\n'))
		n++
	}
	if err := s.Err(); err != nil {
		panic(err)
	}
	if err := w.Flush(); err != nil {
		panic(fmt.Sprintf("flush output: %v", err))
	}
	fmt.Printf("wrote %d records to %s\n", n, outPath)
}
