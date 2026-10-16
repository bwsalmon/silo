// Command genschema writes silofile.schema.json from the Silofile's Go
// types. Run it with go generate ./silofile.
package main

import (
	"log"
	"os"

	"github.com/bwsalmon/silo/old/silofile"
)

func main() {
	b, err := silofile.GenerateSchema()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("silofile.schema.json", b, 0o644); err != nil {
		log.Fatal(err)
	}
}
