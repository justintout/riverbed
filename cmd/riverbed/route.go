package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/justintout/riverbed"
	"github.com/justintout/riverbed/route"
)

// routeCmd reports how a transcription would be routed, without storing it or
// calling an agent.
//
// Semantic thresholds cannot be chosen in the abstract, because the usable range
// depends on the embedding model. This prints the score of every semantic rule
// so a threshold can be read off real examples.
func routeCmd(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	var c common
	c.bind(fs)
	scores := fs.Bool("scores", true, "show the score of every semantic rule")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: riverbed route [flags] <transcription>")
		fmt.Fprintln(os.Stderr, "       riverbed route [flags] -          # read lines from stdin")
		fmt.Fprintln(os.Stderr, "\nReports the route a transcription would take. Nothing is stored.")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("give a transcription, or - to read from stdin")
	}

	cfg, logger, err := c.load()
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	app, err := riverbed.Open(ctx, riverbed.OpenOptions{
		Config: cfg, Version: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	router := app.Router()

	if fs.Arg(0) == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if err := explain(ctx, router, line, *scores); err != nil {
				return err
			}
		}
		return scanner.Err()
	}

	return explain(ctx, router, strings.Join(fs.Args(), " "), *scores)
}

func explain(ctx context.Context, router *route.Router, text string, showScores bool) error {
	decision, matches, err := router.Explain(ctx, text)
	if err != nil {
		return err
	}

	fmt.Printf("%q\n", text)
	fmt.Printf("  route:  %s\n", decision.Agent)
	fmt.Printf("  reason: %s\n", decision.Reason)
	if decision.Prompt != text {
		fmt.Printf("  prompt: %q\n", decision.Prompt)
	}
	if len(decision.Tags) > 0 {
		fmt.Printf("  tags:   %s\n", strings.Join(decision.Tags, ", "))
	}

	if showScores && len(matches) > 0 {
		fmt.Println("  semantic scores:")
		for _, match := range matches {
			mark := " "
			if match.Matched() {
				mark = "*"
			}
			fmt.Printf("    %s %-12s %.3f  (threshold %.2f, closest %q)\n",
				mark, match.Agent, match.Score, match.Threshold, match.Utterance)
		}
	}
	fmt.Println()
	return nil
}
