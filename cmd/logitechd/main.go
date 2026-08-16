package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/daemon"
	"github.com/atremb/logitechd/internal/power"
	"github.com/atremb/logitechd/internal/receiver"
)

var (
	version = "dev"
	commit  = "unknown"
)

const defaultConfigPath = "/etc/logitechd/config.yaml"

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to the strict YAML configuration")
	receiverPath := flag.String("receiver-path", "", "override the configured receiver HIDRAW path")
	validate := flag.Bool("validate", false, "validate configuration without opening hardware")
	validateConfig := flag.Bool("validate-config", false, "alias for -validate")
	dryRun := flag.Bool("dry-run", false, "alias for -validate")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("logitechd %s (commit %s)\n", version, commit)
		return
	}

	settings, err := config.LoadFile(*configPath)
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(2)
	}
	if *receiverPath != "" {
		settings.Receiver.Path = *receiverPath
		if err := settings.Validate(); err != nil {
			log.Printf("configuration error: %v", err)
			os.Exit(2)
		}
	}
	if *validate || *validateConfig || *dryRun {
		if err := daemon.ValidateConfig(settings); err != nil {
			log.Printf("configuration error: %v", err)
			os.Exit(2)
		}
		log.Printf("configuration is valid; hardware was not opened")
		return
	}

	log.Printf("starting logitechd: version=%s commit=%s", version, commit)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	resumeEvents, err := power.WatchResumes(ctx)
	if err != nil {
		log.Printf("host resume monitoring temporarily unavailable; retrying: %v", err)
	}
	run, err := daemon.New(settings, daemon.Options{
		Receiver: receiver.Options{
			Path: settings.Receiver.Path,
			Kind: receiverKind(settings.Receiver.Type),
		},
		ResumeEvents: resumeEvents,
		Logger:       log.Default(),
	})
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(2)
	}
	if err := run.Run(ctx); err != nil {
		log.Printf("daemon stopped: %v", err)
		os.Exit(1)
	}
}

func receiverKind(value config.ReceiverType) receiver.Kind {
	switch value {
	case config.ReceiverUnifying:
		return receiver.KindUnifying
	case config.ReceiverBolt:
		return receiver.KindBolt
	default:
		return receiver.KindUnknown
	}
}
