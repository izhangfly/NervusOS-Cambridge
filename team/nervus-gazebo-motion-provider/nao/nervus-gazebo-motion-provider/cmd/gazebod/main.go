package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/app"
	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/jetty"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-gazebo-provider/internal/provider"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simprocess"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-gazebo-provider/internal/wslg"
	"github.com/nervus-os/nervus-ipc/sdk"
)

func main() {
	socketPath := flag.String("sock", sdk.DefaultSockPath, "nervud control-plane Unix socket")
	modeName := flag.String("mode", "attach", "simulation process mode: attach or managed")
	world := flag.String("world", "nervus_home", "expected Gazebo world name")
	partition := flag.String("gz-partition", os.Getenv("GZ_PARTITION"), "Gazebo Transport partition")
	profileRoot := flag.String("profile-root", "profiles", "allowlisted root containing signed motion Profiles")
	profileBundle := flag.String("profile-bundle", "nao-fixed-v1.json", "signed Profile bundle relative to profile-root")
	runtimeRoot := flag.String("runtime-config-root", "profiles", "allowlisted root containing runtime instance configuration")
	runtimeFile := flag.String("runtime-config", "runtime-nao-fixed-v1.json", "runtime instance configuration relative to runtime-config-root")
	worldFile := flag.String("world-file", "", "managed mode SDF path for the default logical world")
	worldRoot := flag.String("world-root", "", "managed mode allowlisted world root")
	gazeboExecutable := flag.String("gazebo-executable", "gz", "managed mode Gazebo launcher executable")
	managedHeadless := flag.Bool("managed-headless", false, "start managed Gazebo server without GUI")
	managedStopGrace := flag.Duration("managed-stop-grace", 5*time.Second, "graceful managed process stop timeout")
	monitorWSLg := flag.Bool("monitor-wslg", true, "report read-only WSLg environment faults")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, or error")
	flag.Parse()

	mode, err := parseMode(*modeName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	profiles, err := motionprofile.LoadBundle(
		*profileRoot, *profileBundle, contract.MotionProfileAllowlist(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gazebod: load motion Profiles: %v\n", err)
		os.Exit(2)
	}
	var runtimeConfig = (*profilev1.RuntimeConfiguration)(nil)
	if *runtimeFile != "" {
		runtimeConfig, err = motionprofile.LoadRuntime(*runtimeRoot, *runtimeFile, profiles)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gazebod: load runtime configuration: %v\n", err)
			os.Exit(2)
		}
		if runtimeConfig.GetWorld() != "" {
			*world = runtimeConfig.GetWorld()
		}
		if runtimeConfig.GetPartition() != "" {
			*partition = runtimeConfig.GetPartition()
		}
		if runtimeConfig.GetLogLevel() != "" {
			*logLevel = runtimeConfig.GetLogLevel()
		}
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "gazebod: invalid log level %q: %v\n", *logLevel, err)
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if os.Getenv("GZ_IP") == "" {
		if err := os.Setenv("GZ_IP", "127.0.0.1"); err != nil {
			log.Error("gazebod: set default GZ_IP", "err", err)
			os.Exit(1)
		}
	}
	transportConfig := gztransport.DefaultConfig()
	transportConfig.Partition = *partition
	client, err := gztransport.Open(transportConfig)
	if err != nil {
		log.Error("gazebod: open Gazebo Transport", "err", err)
		os.Exit(1)
	}
	jettyConfig := jetty.DefaultConfig()
	jettyConfig.Mode = mode
	jettyConfig.World = *world
	jettyConfig.Profiles = profiles
	jettyConfig.Runtime = runtimeConfig
	if runtimeConfig != nil {
		if runtimeConfig.GetDiscoveryTimeoutMs() != 0 {
			jettyConfig.DiscoveryTimeout = time.Duration(runtimeConfig.GetDiscoveryTimeoutMs()) * time.Millisecond
		}
		if runtimeConfig.GetRequestTimeoutMs() != 0 {
			jettyConfig.RequestTimeout = time.Duration(runtimeConfig.GetRequestTimeoutMs()) * time.Millisecond
		}
	}
	var process simulation.ProcessController
	if mode == simulation.ModeManaged {
		if *worldFile == "" || *worldRoot == "" {
			_ = client.Close()
			log.Error("gazebod: managed mode requires -world-file and -world-root")
			os.Exit(2)
		}
		args := []string{"sim", "-r"}
		if *managedHeadless {
			args = append(args, "-s")
		}
		process, err = simprocess.NewManaged(simprocess.Config{
			Executable: *gazeboExecutable,
			BaseArgs:   args,
			Worlds:     map[string]string{*world: *worldFile},
			WorldRoots: []string{*worldRoot},
			StopGrace:  *managedStopGrace,
			Stdout:     os.Stdout,
			Stderr:     os.Stderr,
		})
		if err != nil {
			_ = client.Close()
			log.Error("gazebod: create managed process controller", "err", err)
			os.Exit(2)
		}
	} else {
		process = simprocess.NewAttach([]string{*world})
	}
	var wslgProbe simulation.WSLgProbe
	if *monitorWSLg {
		probeConfig := wslg.DefaultConfig()
		if *managedHeadless {
			probeConfig.RequireDisplay = false
			probeConfig.RequireKeepalive = false
			probeConfig.RequireSharedMemory = false
			probeConfig.RequireCopyModeCheck = false
		}
		wslgProbe, err = wslg.New(probeConfig)
		if err != nil {
			_ = process.Close()
			_ = client.Close()
			log.Error("gazebod: create WSLg probe", "err", err)
			os.Exit(2)
		}
	}
	backend, err := jetty.NewWithLifecycle(jettyConfig, client, process, wslgProbe, log)
	if err != nil {
		_ = process.Close()
		_ = client.Close()
		log.Error("gazebod: create Jetty backend", "err", err)
		os.Exit(1)
	}
	if err := app.Run(ctx, app.Config{
		SocketPath: *socketPath,
		Provider:   provider.DefaultConfig(),
	}, backend, log); err != nil {
		log.Error("gazebod exited", "err", err)
		os.Exit(1)
	}
	log.Info("gazebod stopped")
}

func parseMode(value string) (simulation.Mode, error) {
	switch value {
	case "attach":
		return simulation.ModeAttach, nil
	case "managed":
		return simulation.ModeManaged, nil
	default:
		return simulation.ModeUnspecified, fmt.Errorf(
			"gazebod: invalid mode %q; expected attach or managed", value,
		)
	}
}
