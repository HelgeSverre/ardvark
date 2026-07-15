// Command profexport runs the export core under CPU profiling against an
// existing database. Not part of the product; not shipped.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/store"
)

func main() {
	dbPath := flag.String("db", "ardvark.db", "sqlite database path")
	format := flag.String("format", "jsonl", "jsonl or csv")
	out := flag.String("out", "", "output file (default: discard)")
	cpuprofile := flag.String("cpuprofile", "", "write CPU profile")
	flag.Parse()

	st, err := store.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	var w io.Writer = io.Discard
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}

	start := time.Now()
	res, err := jsonout.Export(st, *format, "", w)
	if err != nil {
		log.Fatal(err)
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("rows=%d format=%s total=%s heapSys=%dMB totalAlloc=%dMB\n",
		res.Rows, *format, time.Since(start).Round(time.Millisecond),
		ms.HeapSys/(1<<20), ms.TotalAlloc/(1<<20))
}
