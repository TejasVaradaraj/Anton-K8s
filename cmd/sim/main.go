// sim is one binary with three roles, so the whole data plane ships as one image:
//
//	sim replica  - a mock vLLM replica with a per-session KV cache
//	sim tier     - the cache tier (KV blocks + workspace snapshots)
//	sim agent    - the sandbox stub: an "agent" that keeps writing to its workspace
//	sim sleep N  - used as a preStop hook (the image has no shell)
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: sim replica|tier|agent|sleep N")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "replica":
		runReplica()
	case "tier":
		runTier()
	case "agent":
		runAgent()
	case "sleep":
		n, _ := strconv.Atoi(os.Args[2])
		time.Sleep(time.Duration(n) * time.Second)
	default:
		fmt.Println("unknown role", os.Args[1])
		os.Exit(2)
	}
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
