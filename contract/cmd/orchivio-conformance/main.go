// Command orchivio-conformance checks a running adapter against the plugin.v1 contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

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
	timeout := flag.Duration("timeout", time.Minute, "overall time limit")
	flag.Parse()

	if message := missingFlag(target); message != "" {
		fmt.Fprintln(os.Stderr, message)
		flag.Usage()
		os.Exit(2)
	}
	target.Notify = func(message string) { fmt.Println(message) }

	if credentialFile != "" {
		data, err := os.ReadFile(credentialFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		target.Credential = data
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	failures := conformance.Run(ctx, target)
	for _, failure := range failures {
		fmt.Printf("FAIL %s: %s\n", failure.Check, failure.Detail)
	}
	if len(failures) > 0 {
		cancel()
		os.Exit(1)
	}
	fmt.Println("conform")
}

func missingFlag(target conformance.Target) string {
	switch {
	case target.Addr == "":
		return "missing -addr"
	case target.Kind == conformance.KindTracker && target.Ticket == "":
		return "missing -ticket for kind tracker"
	case target.Kind == conformance.KindCodeHost && target.Repo == "":
		return "missing -repo for kind codehost"
	case target.Kind != conformance.KindTracker && target.Kind != conformance.KindCodeHost:
		return "missing or unknown -kind (tracker or codehost)"
	}
	return ""
}
