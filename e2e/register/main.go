// Command register performs synthetic source-identity registration for
// E2E capability scenarios: it binds one namespace plus capability
// tokens to a source scope through the existing store API. It must run
// while no bridge owns the database (before bridge start or after stop);
// the bridge never negotiates capability from payloads, so the harness
// seeds the contract explicitly instead. Synthetic registration proves
// bridge behavior under a configured contract, never upstream I1
// interoperability.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

func main() {
	db := flag.String("db", "", "database path (required)")
	scope := flag.String("scope", "", "source scope (required)")
	namespace := flag.String("namespace", "", "identity namespace (required)")
	capabilities := flag.String("capabilities", "", "comma-separated capability tokens")
	flag.Parse()
	if *db == "" || *scope == "" || *namespace == "" {
		log.Fatal("db, scope, and namespace are required")
	}
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		if err := st.Close(); err != nil {
			log.Fatal(err)
		}
	}()
	if err := st.BindSourceScope(ctx, *scope); err != nil {
		log.Fatal(err)
	}
	if err := st.BindSourceIdentity(ctx, *scope, *namespace, *capabilities); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stdout.WriteString("registered scope=" + *scope + "\n"); err != nil {
		log.Fatal(err)
	}
}
