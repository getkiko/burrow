package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/getkiko/burrow/diag"
	"github.com/urfave/cli/v2"
)

func TestBirthdayCommandHelp(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output

	if err := app.Run([]string{"cli", "birthday", "--help"}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"server", "client"} {
		if !strings.Contains(output.String(), command) {
			t.Fatalf("birthday help missing %q:\n%s", command, output.String())
		}
	}
}

func TestBirthdayServerHelpHidesClientOwnedParameters(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output
	if err := app.Run([]string{"cli", "birthday", "server", "--help"}); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"--candidates", "--trials", "--min-port", "--max-port", "--warmup", "--attempt-timeout", "--probe-ports"} {
		if strings.Contains(output.String(), hidden) {
			t.Fatalf("server help exposes client-owned or legacy flag %s:\n%s", hidden, output.String())
		}
	}
}

func TestBirthdayServerDefaultsToContinuousService(t *testing.T) {
	for _, flag := range newBirthdayServerCommand().Flags {
		if flag.Names()[0] != "trials" {
			continue
		}
		trials, ok := flag.(*cli.IntFlag)
		if !ok || trials.Value != 0 || !trials.Hidden {
			t.Fatalf("server trials flag = %#v", flag)
		}
		return
	}
	t.Fatal("server trials flag missing")
}

func TestBirthdayCommandsAcceptSingleDashLongFlags(t *testing.T) {
	app := newApp()
	err := app.Run([]string{"cli", "birthday", "client", "-signal", "203.0.113.10:9960/signal", "-candidates", "0"})
	if err == nil || !strings.Contains(err.Error(), "candidates and trials must be greater than 0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBirthdayCommandsRequireSignal(t *testing.T) {
	for _, command := range []string{"server", "client"} {
		t.Run(command, func(t *testing.T) {
			app := newApp()
			err := app.Run([]string{"cli", "birthday", command, "-room", "test"})
			if err == nil || !strings.Contains(err.Error(), "signal") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPunchCommandHelp(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output

	if err := app.Run([]string{"cli", "punch", "--help"}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"server", "client"} {
		if !strings.Contains(output.String(), command) {
			t.Fatalf("punch help missing %q:\n%s", command, output.String())
		}
	}
}

// The punch commands must not default -iface to a platform-specific name
// such as en0: an empty value lets the kernel routing table choose, which
// works on every platform.
func TestPunchCommandsDefaultToAutomaticInterface(t *testing.T) {
	for _, command := range []*cli.Command{newPunchServerCommand(), newPunchClientCommand()} {
		found := false
		for _, flag := range command.Flags {
			stringFlag, ok := flag.(*cli.StringFlag)
			if !ok || stringFlag.Name != "iface" {
				continue
			}
			found = true
			if stringFlag.Value != "" {
				t.Fatalf("punch %s -iface default = %q, want empty", command.Name, stringFlag.Value)
			}
		}
		if !found {
			t.Fatalf("punch %s has no -iface flag", command.Name)
		}
	}
}

func TestPunchCommandsRequireSignal(t *testing.T) {
	for _, command := range []string{"server", "client"} {
		t.Run(command, func(t *testing.T) {
			app := newApp()
			err := app.Run([]string{"cli", "punch", command, "-room", "test"})
			if err == nil || !strings.Contains(err.Error(), "signal") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestTCPNATCommandAcceptsSingleDashLongFlags(t *testing.T) {
	for _, signalPathFlag := range []string{"-signal_path", "-signal-path"} {
		t.Run(signalPathFlag, func(t *testing.T) {
			app := newApp()
			err := app.Run([]string{"cli", "tcpnat", "-server", "127.0.0.1", signalPathFlag, "/signal", "-rounds", "0"})
			if err == nil || !strings.Contains(err.Error(), "rounds must be greater than 0") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSTUNCommandUsesMiwifiByDefault(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output
	if err := app.Run([]string{"cli", "stun", "--help"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--stun", diag.DefaultSTUNServer, "--iface", "--timeout"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("stun help missing %q:\n%s", expected, output.String())
		}
	}
}

func TestAPICommandHelp(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output
	if err := app.Run([]string{"cli", "api", "--help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--listen") {
		t.Fatalf("api help missing listen flag:\n%s", output.String())
	}
	if !strings.Contains(output.String(), defaultAPIListenAddress) {
		t.Fatalf("api help missing default listen address %s:\n%s", defaultAPIListenAddress, output.String())
	}
}

func TestSignalPathFlagIsAvailableToIntegratedCommands(t *testing.T) {
	tests := [][]string{
		{"tcpnat"},
		{"punch", "client"},
		{"punch", "server"},
		{"birthday", "client"},
		{"birthday", "server"},
	}
	for _, path := range tests {
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			var output bytes.Buffer
			app := newApp()
			app.Writer = &output
			app.ErrWriter = &output
			args := append([]string{"cli"}, path...)
			args = append(args, "--help")
			if err := app.Run(args); err != nil {
				t.Fatal(err)
			}
			for _, flag := range []string{"--signal_path", "--signal-path"} {
				if !strings.Contains(output.String(), flag) {
					t.Fatalf("%s help missing %s:\n%s", strings.Join(path, " "), flag, output.String())
				}
			}
		})
	}
}
