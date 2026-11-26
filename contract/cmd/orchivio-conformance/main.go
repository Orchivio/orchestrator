// Command orchivio-conformance checks a running adapter against the plugin.v1 contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/orchivio/orchestrator/contract/conformance"
)

func main() {
	var target conformance.Target
	var credentialFile string
	flag.StringVar(&target.Addr, "addr", "", "adapter address host:port")
	flag.StringVar(&target.Kind, "kind", "", "tracker or codehost")
	flag.StringVar(&credentialFile, "credential-file", "", "file with the credential bytes")
	flag.StringVar(&target.Ticket, "ticket", "", "existing ticket reference (tracker)")
	flag.StringVar(&target.Repo, "repo", "", "existing repository (codehost)")
	flag.StringVar(&target.Status, "status", "", "status for the SetStatus check (tracker, optional)")
	flag.Parse()

	if credentialFile != "" {
		data, err := os.ReadFile(credentialFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		target.Credential = data
	}
	failures := conformance.Run(context.Background(), target)
	for _, failure := range failures {
		fmt.Printf("FAIL %s: %s\n", failure.Check, failure.Detail)
	}
	if len(failures) > 0 {
		os.Exit(1)
	}
	fmt.Println("conform")
}
