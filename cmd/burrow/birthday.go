package main

import (
	"fmt"
	"io"
	"time"

	"github.com/getkiko/burrow/internal/birthdaylab"
	"github.com/urfave/cli/v2"
)

func newBirthdayCommand() *cli.Command {
	return &cli.Command{
		Name:  "birthday",
		Usage: "run the TCP birthday-collision experiment",
		Subcommands: []*cli.Command{
			newBirthdayServerCommand(),
			newBirthdayClientCommand(),
		},
	}
}

func validateExperiment(candidates, trials, minPort, maxPort int) (int, error) {
	if candidates <= 0 || trials <= 0 {
		return 0, fmt.Errorf("candidates and trials must be greater than 0")
	}
	portCount := maxPort - minPort + 1
	if portCount <= 0 || candidates > portCount {
		return 0, fmt.Errorf("invalid port range or candidate count")
	}
	return portCount, nil
}

func printTrialResult(output io.Writer, result birthdaylab.TrialResult, stats birthdaylab.Statistics) {
	diagnostics := result.Diagnostics
	errors := diagnostics.DialErrors
	fmt.Fprintf(output,
		"  diagnostics armed=%d/%d connected=%d validated=%d validation-errors=%d dial-errors={timeout:%d canceled:%d refused:%d resource:%d other:%d}\n",
		diagnostics.Armed,
		diagnostics.Requested,
		diagnostics.Connected,
		diagnostics.Validated,
		diagnostics.ValidationErrors,
		errors.Timeout,
		errors.Canceled,
		errors.Refused,
		errors.Resource,
		errors.Other,
	)
	if result.Success {
		fmt.Fprintf(output, "  result=SUCCESS local=%s remote=%s elapsed=%s\n", result.LocalAddr, result.RemoteAddr, result.Elapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintf(output, "  result=FAILED elapsed=%s\n", result.Elapsed.Round(time.Millisecond))
	}
	fmt.Fprintf(output, "  cumulative=%d/%d rate=%.2f%% average=%s\n\n",
		stats.Successes, stats.Trials, stats.SuccessRate()*100, stats.AverageElapsed().Round(time.Millisecond))
}

func printTotal(output io.Writer, role string, stats birthdaylab.Statistics, theory float64) {
	fmt.Fprintf(output, "[%s total] successes=%d trials=%d empirical=%.2f%% theory=%.2f%% three-attempt=%.2f%%\n",
		role,
		stats.Successes,
		stats.Trials,
		stats.SuccessRate()*100,
		theory*100,
		birthdaylab.RepeatedProbability(stats.SuccessRate(), 3)*100,
	)
}

func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
