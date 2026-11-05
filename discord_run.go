package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type discordOptions struct {
	Config discordConfig
	N      int
	Find   int
	Fresh  bool
	Out    string
	Delay  time.Duration
}

type discordChecker interface {
	cached(string) (verification, bool)
	check(context.Context, string, bool) (verification, error)
}

func runDiscord(ctx context.Context, opts discordOptions, out, log io.Writer) error {
	if opts.N < 0 || opts.Find < 0 || opts.Delay < time.Second || opts.Out == "" {
		return errors.New("Discord requires -n >= 0, -find >= 0, -delay >= 1s, and a nonempty -out")
	}
	if sameFilePath(opts.Config.SourcePath, opts.Out) {
		return errors.New("the Discord results file must differ from the input wordlist")
	}
	names, err := discordNames(opts.Config)
	if err != nil {
		return err
	}
	session, err := openDiscordSession(opts.Out, opts.Config)
	if err != nil {
		return err
	}
	defer session.Close()
	session.client.interval = opts.Delay
	return runDiscordChecks(ctx, session, names, opts, out, log)
}

func runDiscordChecks(ctx context.Context, session discordChecker, names []string, opts discordOptions, out, log io.Writer) error {
	fmt.Fprintf(log, "Discord: %d source names; delay %s. Availability is an observation, not a reservation.\n", len(names), opts.Delay)
	checked, available, reused := 0, 0, 0
	defer func() {
		fmt.Fprintf(log, "Discord: %d new checks, %d cached observations, %d available names.\n", checked, reused, available)
	}()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, cached := session.cached(name)
		if opts.Fresh {
			cached = false
		}
		if cached {
			reused++
		} else {
			if opts.N > 0 && checked >= opts.N {
				break
			}
			var err error
			result, err = session.check(ctx, name, opts.Fresh)
			if err != nil {
				return fmt.Errorf("Discord %s: %w", name, err)
			}
			checked++
		}
		if result.Name != name || (result.Status != "available" && result.Status != "unavailable" && result.Status != "invalid") {
			return errors.New("Discord returned an inconclusive observation")
		}
		if result.Status == "invalid" {
			fmt.Fprintf(log, "Discord skipped invalid name: %s\n", name)
		}
		if result.Status == "available" {
			if _, err := fmt.Fprintln(out, name); err != nil {
				return err
			}
			available++
			if opts.Find > 0 && available >= opts.Find {
				return nil
			}
		}
		if checked > 0 && checked%25 == 0 && !cached {
			fmt.Fprintf(log, "Discord: %d new checks; %d available.\n", checked, available)
		}
	}
	return nil
}
