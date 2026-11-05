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
	Resume bool
	Out    string
	Delay  time.Duration
}

type discordChecker interface {
	cached(string) (verification, bool)
	check(context.Context, string, bool) (verification, error)
}

type recordedDiscordChecker interface {
	recorded(string) (verification, bool)
}

type publishingDiscordChecker interface {
	needsPublish(string) bool
	markPublished(verification) error
}

func runDiscord(ctx context.Context, opts discordOptions, out, log io.Writer) error {
	if opts.N < 0 || opts.Find < 0 || opts.Delay < time.Second || opts.Out == "" {
		return errors.New("Discord requires -n >= 0, -find >= 0, -delay >= 1s, and a nonempty -out")
	}
	if sameFilePath(opts.Config.SourcePath, opts.Out) {
		return errors.New("the Discord results file must differ from the input wordlist")
	}
	names, err := discordSource(opts.Config)
	if err != nil {
		return err
	}
	session, err := openDiscordSession(opts.Out, opts.Config)
	if err != nil {
		return err
	}
	defer session.Close()
	session.client.interval = opts.Delay
	available := 0
	for _, result := range session.cache {
		if result.Status == "available" {
			available++
		}
	}
	progressSource(ctx, names.Len(), len(session.cache))
	progressVerification(ctx, len(session.cache), available)
	gate, err := runtimeProviderGate("discord-eligibility", opts.Delay)
	if err != nil {
		return err
	}
	session.client.gate = gate
	return runDiscordSourceChecks(ctx, session, names, opts, out, log)
}

func runDiscordChecks(ctx context.Context, session discordChecker, names []string, opts discordOptions, out, log io.Writer) error {
	return runDiscordSourceChecks(ctx, session, &nameSource{words: names}, opts, out, log)
}

func runDiscordSourceChecks(ctx context.Context, session discordChecker, names *nameSource, opts discordOptions, out, log io.Writer) error {
	fmt.Fprintf(log, "Discord: %d source names; delay %s. Availability is an observation, not a reservation.\n", names.Len(), opts.Delay)
	checked, available, reused := 0, 0, 0
	defer func() {
		fmt.Fprintf(log, "Discord: %d new checks, %d cached observations, %d available names.\n", checked, reused, available)
	}()
	for i := 0; i < names.Len(); i++ {
		name := names.At(i)
		if err := ctx.Err(); err != nil {
			return err
		}
		result, cached := session.cached(name)
		publisher, publishes := session.(publishingDiscordChecker)
		if opts.Resume && !opts.Fresh {
			if history, ok := session.(recordedDiscordChecker); ok {
				if previous, exists := history.recorded(name); exists {
					if previous.Status == "unavailable" || previous.Status == "invalid" {
						result, cached = previous, true
					} else if previous.Status == "available" && publishes && !publisher.needsPublish(name) {
						// This pass already delivered the observation. Rechecking old
						// positives after each cooldown would starve unsearched names.
						reused++
						continue
					}
				}
			}
		}
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
			if err := publishResult(out, name); err != nil {
				return err
			}
			if publishes {
				if err := publisher.markPublished(result); err != nil {
					return err
				}
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
