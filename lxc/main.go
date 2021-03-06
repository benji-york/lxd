package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/lxc/lxd"
	"github.com/lxc/lxd/internal/gnuflag"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

var verbose = gnuflag.Bool("v", false, "Enables verbose mode.")
var debug = gnuflag.Bool("debug", false, "Enables debug mode.")
var configPath = gnuflag.String("config", "", "Alternate config path.")

func run() error {
	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		os.Args[1] = "help"
	}
	if len(os.Args) < 2 || os.Args[1] == "" || os.Args[1][0] == '-' {
		return fmt.Errorf("missing subcommand")
	}
	name := os.Args[1]
	cmd, ok := commands[name]
	if !ok {
		return fmt.Errorf("unknown command: %s", name)
	}
	cmd.flags()
	gnuflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s\n\nOptions:\n\n", strings.TrimSpace(cmd.usage()))
		gnuflag.PrintDefaults()
	}

	os.Args = os.Args[1:]
	gnuflag.Parse(true)

	if *verbose || *debug {
		lxd.SetLogger(log.New(os.Stderr, "", log.LstdFlags))
		lxd.SetDebug(*debug)
	}

	config, err := lxd.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	return cmd.run(config, gnuflag.Args())
}

type command interface {
	usage() string
	flags()
	run(config *lxd.Config, args []string) error
}

var commands = map[string]command{
	"version":  &versionCmd{},
	"help":     &helpCmd{},
	"finger":   &fingerCmd{},
	"config":   &configCmd{},
	"create":   &createCmd{},
	"list":     &listCmd{},
	"shell":    &shellCmd{},
	"remote":   &remoteCmd{},
	"stop":     &actionCmd{lxd.Stop},
	"start":    &actionCmd{lxd.Start},
	"restart":  &actionCmd{lxd.Restart},
	"freeze":   &actionCmd{lxd.Freeze},
	"unfreeze": &actionCmd{lxd.Unfreeze},
	"delete":   &deleteCmd{},
	"file":     &fileCmd{},
	"snapshot": &snapshotCmd{},
}

var errArgs = fmt.Errorf("too many subcommand arguments")
