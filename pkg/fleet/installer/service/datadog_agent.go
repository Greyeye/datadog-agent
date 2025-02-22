// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !windows

// Package service provides a way to interact with os services
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/DataDog/datadog-agent/pkg/fleet/installer/repository"
	"github.com/DataDog/datadog-agent/pkg/fleet/internal/cdn"
	"github.com/DataDog/datadog-agent/pkg/util/installinfo"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

const (
	agentPackage            = "datadog-agent"
	pathOldAgent            = "/opt/datadog-agent"
	agentSymlink            = "/usr/bin/datadog-agent"
	agentUnit               = "datadog-agent.service"
	traceAgentUnit          = "datadog-agent-trace.service"
	processAgentUnit        = "datadog-agent-process.service"
	systemProbeUnit         = "datadog-agent-sysprobe.service"
	securityAgentUnit       = "datadog-agent-security.service"
	agentExp                = "datadog-agent-exp.service"
	traceAgentExp           = "datadog-agent-trace-exp.service"
	processAgentExp         = "datadog-agent-process-exp.service"
	systemProbeExp          = "datadog-agent-sysprobe-exp.service"
	securityAgentExp        = "datadog-agent-security-exp.service"
	configDatadogYAML       = "datadog.yaml"
	configSecurityAgentYAML = "security-agent.yaml"
	configSystemProbeYAML   = "system-probe.yaml"
)

var (
	stableUnits = []string{
		agentUnit,
		traceAgentUnit,
		processAgentUnit,
		systemProbeUnit,
		securityAgentUnit,
	}
	experimentalUnits = []string{
		agentExp,
		traceAgentExp,
		processAgentExp,
		systemProbeExp,
		securityAgentExp,
	}
)

var (
	// matches omnibus/package-scripts/agent-deb/postinst
	rootOwnedAgentPaths = []string{
		"embedded/bin/system-probe",
		"embedded/bin/security-agent",
		"embedded/share/system-probe/ebpf",
		"embedded/share/system-probe/java",
	}
)

// SetupAgent installs and starts the agent
func SetupAgent(ctx context.Context, _ []string) (err error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "setup_agent")
	defer func() {
		if err != nil {
			log.Errorf("Failed to setup agent, reverting: %s", err)
			err = errors.Join(err, RemoveAgent(ctx))
		}
		span.Finish(tracer.WithError(err))
	}()

	if err = stopOldAgentUnits(ctx); err != nil {
		return err
	}

	for _, unit := range stableUnits {
		if err = loadUnit(ctx, unit); err != nil {
			return fmt.Errorf("failed to load %s: %v", unit, err)
		}
	}
	for _, unit := range experimentalUnits {
		if err = loadUnit(ctx, unit); err != nil {
			return fmt.Errorf("failed to load %s: %v", unit, err)
		}
	}
	if err = os.MkdirAll("/etc/datadog-agent", 0755); err != nil {
		return fmt.Errorf("failed to create /etc/datadog-agent: %v", err)
	}
	ddAgentUID, ddAgentGID, err := getAgentIDs()
	if err != nil {
		return fmt.Errorf("error getting dd-agent user and group IDs: %w", err)
	}

	if err = os.Chown("/etc/datadog-agent", ddAgentUID, ddAgentGID); err != nil {
		return fmt.Errorf("failed to chown /etc/datadog-agent: %v", err)
	}
	if err = chownRecursive("/opt/datadog-packages/datadog-agent/stable/", ddAgentUID, ddAgentGID, rootOwnedAgentPaths); err != nil {
		return fmt.Errorf("failed to chown /opt/datadog-packages/datadog-agent/stable/: %v", err)
	}

	if err = systemdReload(ctx); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %v", err)
	}

	// enabling the agentUnit only is enough as others are triggered by it
	if err = enableUnit(ctx, agentUnit); err != nil {
		return fmt.Errorf("failed to enable %s: %v", agentUnit, err)
	}
	if err = exec.CommandContext(ctx, "ln", "-sf", "/opt/datadog-packages/datadog-agent/stable/bin/agent/agent", agentSymlink).Run(); err != nil {
		return fmt.Errorf("failed to create symlink: %v", err)
	}

	// write installinfo before start, or the agent could write it
	// TODO: add installer version properly
	if err = installinfo.WriteInstallInfo("installer_package", "manual_update"); err != nil {
		return fmt.Errorf("failed to write install info: %v", err)
	}

	_, err = os.Stat("/etc/datadog-agent/datadog.yaml")
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to check if /etc/datadog-agent/datadog.yaml exists: %v", err)
	}
	// this is expected during a fresh install with the install script / asible / chef / etc...
	// the config is populated afterwards by the install method and the agent is restarted
	if !os.IsNotExist(err) {
		if err = startUnit(ctx, agentUnit); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAgent stops and removes the agent
func RemoveAgent(ctx context.Context) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "remove_agent_units")
	defer span.Finish()
	// stop experiments, they can restart stable agent
	for _, unit := range experimentalUnits {
		if err := stopUnit(ctx, unit); err != nil {
			log.Warnf("Failed to stop %s: %s", unit, err)
		}
	}
	// stop stable agents
	for _, unit := range stableUnits {
		if err := stopUnit(ctx, unit); err != nil {
			log.Warnf("Failed to stop %s: %s", unit, err)
		}
	}

	if err := disableUnit(ctx, agentUnit); err != nil {
		log.Warnf("Failed to disable %s: %s", agentUnit, err)
	}

	// remove units from disk
	for _, unit := range experimentalUnits {
		if err := removeUnit(ctx, unit); err != nil {
			log.Warnf("Failed to remove %s: %s", unit, err)
		}
	}
	for _, unit := range stableUnits {
		if err := removeUnit(ctx, unit); err != nil {
			log.Warnf("Failed to remove %s: %s", unit, err)
		}
	}
	if err := os.Remove(agentSymlink); err != nil {
		log.Warnf("Failed to remove agent symlink: %s", err)
	}
	installinfo.RmInstallInfo()
	// TODO: Return error to caller?
	return nil
}

func oldAgentInstalled() bool {
	_, err := os.Stat(pathOldAgent)
	return err == nil
}

func stopOldAgentUnits(ctx context.Context) error {
	if !oldAgentInstalled() {
		return nil
	}
	span, ctx := tracer.StartSpanFromContext(ctx, "remove_old_agent_units")
	defer span.Finish()
	for _, unit := range stableUnits {
		if err := stopUnit(ctx, unit); err != nil {
			return fmt.Errorf("failed to stop %s: %v", unit, err)
		}
		if err := disableUnit(ctx, unit); err != nil {
			return fmt.Errorf("failed to disable %s: %v", unit, err)
		}
	}
	return nil
}

func chownRecursive(path string, uid int, gid int, ignorePaths []string) error {
	return filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		for _, ignore := range ignorePaths {
			if relPath == ignore || strings.HasPrefix(relPath, ignore+string(os.PathSeparator)) {
				return nil
			}
		}
		return os.Chown(p, uid, gid)
	})
}

// StartAgentExperiment starts the agent experiment
func StartAgentExperiment(ctx context.Context) error {
	ddAgentUID, ddAgentGID, err := getAgentIDs()
	if err != nil {
		return fmt.Errorf("error getting dd-agent user and group IDs: %w", err)
	}
	if err = chownRecursive("/opt/datadog-packages/datadog-agent/experiment/", ddAgentUID, ddAgentGID, rootOwnedAgentPaths); err != nil {
		return fmt.Errorf("failed to chown /opt/datadog-packages/datadog-agent/experiment/: %v", err)
	}
	return startUnit(ctx, agentExp, "--no-block")
}

// StopAgentExperiment stops the agent experiment
func StopAgentExperiment(ctx context.Context) error {
	return startUnit(ctx, agentUnit)
}

// PromoteAgentExperiment promotes the agent experiment
func PromoteAgentExperiment(ctx context.Context) error {
	return StopAgentExperiment(ctx)
}

// ConfigureAgent configures the stable agent
func ConfigureAgent(ctx context.Context, cdn cdn.CDN, configs *repository.Repositories) error {
	config, err := cdn.Get(ctx)
	if err != nil {
		return fmt.Errorf("could not get cdn config: %w", err)
	}
	tmpDir, err := configs.MkdirTemp()
	if err != nil {
		return fmt.Errorf("could not create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	err = WriteAgentConfig(config, tmpDir)
	if err != nil {
		return fmt.Errorf("could not write agent config: %w", err)
	}

	err = configs.Create(agentPackage, config.Version, tmpDir)
	if err != nil {
		return fmt.Errorf("could not create repository: %w", err)
	}
	return nil
}

// WriteAgentConfig writes the agent configuration to the given directory
func WriteAgentConfig(config *cdn.Config, dir string) error {
	ddAgentUID, ddAgentGID, err := getAgentIDs()
	if err != nil {
		return fmt.Errorf("error getting dd-agent user and group IDs: %w", err)
	}

	if config.Datadog != nil {
		err = os.WriteFile(filepath.Join(dir, configDatadogYAML), []byte(config.Datadog), 0640)
		if err != nil {
			return fmt.Errorf("could not write datadog.yaml: %w", err)
		}
		err = os.Chown(filepath.Join(dir, configDatadogYAML), ddAgentUID, ddAgentGID)
		if err != nil {
			return fmt.Errorf("could not chown datadog.yaml: %w", err)
		}
	}
	if config.SecurityAgent != nil {
		err = os.WriteFile(filepath.Join(dir, configSecurityAgentYAML), []byte(config.SecurityAgent), 0600)
		if err != nil {
			return fmt.Errorf("could not write datadog.yaml: %w", err)
		}
	}
	if config.SystemProbe != nil {
		err = os.WriteFile(filepath.Join(dir, configSystemProbeYAML), []byte(config.SystemProbe), 0600)
		if err != nil {
			return fmt.Errorf("could not write datadog.yaml: %w", err)
		}
	}
	return nil
}
