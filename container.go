// SPDX-License-Identifier: Apache-2.0

// This file is used to handle container checkpoint archives

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/checkpoint-restore/go-criu/v6/crit"
	"github.com/olekukonko/tablewriter"
	spec "github.com/opencontainers/runtime-spec/specs-go"
)

type containerMetadata struct {
	Name    string `json:"name,omitempty"`
	Attempt uint32 `json:"attempt,omitempty"`
}

type containerInfo struct {
	Name    string
	IP      string
	MAC     string
	Created string
	Engine  string
}

func getPodmanInfo(containerConfig *metadata.ContainerConfig, _ *spec.Spec) *containerInfo {
	return &containerInfo{
		Name:    containerConfig.Name,
		Created: containerConfig.CreatedTime.Format(time.RFC3339),
		Engine:  "Podman",
	}
}

func getContainerdInfo(containerdStatus *metadata.ContainerdStatus, specDump *spec.Spec) *containerInfo {
	return &containerInfo{
		Name:    specDump.Annotations["io.kubernetes.cri.container-name"],
		Created: time.Unix(0, containerdStatus.CreatedAt).Format(time.RFC3339),
		Engine:  "containerd",
	}
}

func getCRIOInfo(_ *metadata.ContainerConfig, specDump *spec.Spec) (*containerInfo, error) {
	cm := containerMetadata{}
	if err := json.Unmarshal([]byte(specDump.Annotations["io.kubernetes.cri-o.Metadata"]), &cm); err != nil {
		return nil, fmt.Errorf("failed to read io.kubernetes.cri-o.Metadata: %w", err)
	}

	return &containerInfo{
		IP:      specDump.Annotations["io.kubernetes.cri-o.IP.0"],
		Name:    cm.Name,
		Created: specDump.Annotations["io.kubernetes.cri-o.Created"],
		Engine:  "CRI-O",
	}, nil
}

func showContainerCheckpoint(checkpointDirectory string) error {
	var (
		row []string
		ci  *containerInfo
	)
	containerConfig, _, err := metadata.ReadContainerCheckpointConfigDump(checkpointDirectory)
	if err != nil {
		return err
	}
	specDump, _, err := metadata.ReadContainerCheckpointSpecDump(checkpointDirectory)
	if err != nil {
		return err
	}

	switch m := specDump.Annotations["io.container.manager"]; m {
	case "libpod":
		ci = getPodmanInfo(containerConfig, specDump)
	case "cri-o":
		ci, err = getCRIOInfo(containerConfig, specDump)
	default:
		containerdStatus, _, _ := metadata.ReadContainerCheckpointStatusFile(checkpointDirectory)
		if containerdStatus == nil {
			return fmt.Errorf("unknown container manager found: %s", m)
		}
		ci = getContainerdInfo(containerdStatus, specDump)
	}

	if err != nil {
		return fmt.Errorf("getting container checkpoint information failed: %w", err)
	}

	fmt.Printf("\nDisplaying container checkpoint data from %s\n\n", checkpointDirectory)

	table := tablewriter.NewWriter(os.Stdout)
	header := []string{
		"Container",
		"Image",
		"ID",
		"Runtime",
		"Created",
		"Engine",
	}

	row = append(row, ci.Name)
	row = append(row, containerConfig.RootfsImageName)
	if len(containerConfig.ID) > 12 {
		row = append(row, containerConfig.ID[:12])
	} else {
		row = append(row, containerConfig.ID)
	}

	row = append(row, containerConfig.OCIRuntime)
	row = append(row, ci.Created)

	row = append(row, ci.Engine)
	if ci.IP != "" {
		header = append(header, "IP")
		row = append(row, ci.IP)
	}
	if ci.MAC != "" {
		header = append(header, "MAC")
		row = append(row, ci.MAC)
	}

	size, err := getCheckpointSize(checkpointDirectory)
	if err != nil {
		return err
	}

	header = append(header, "CHKPT Size")
	row = append(row, metadata.ByteToString(size))

	// Display root fs diff size if available
	fi, err := os.Lstat(filepath.Join(checkpointDirectory, metadata.RootFsDiffTar))
	if err == nil {
		if fi.Size() != 0 {
			header = append(header, "Root Fs Diff Size")
			row = append(row, metadata.ByteToString(fi.Size()))
		}
	}

	table.SetAutoMergeCells(true)
	table.SetRowLine(true)
	table.SetHeader(header)
	table.Append(row)
	table.Render()

	if showMounts {
		table = tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{
			"Destination",
			"Type",
			"Source",
		})
		// Get overview of mounts from spec.dump
		for _, data := range specDump.Mounts {
			table.Append([]string{
				data.Destination,
				data.Type,
				func() string {
					if fullPaths {
						return data.Source
					}
					return shortenPath(data.Source)
				}(),
			})
		}
		fmt.Println("\nOverview of Mounts")
		table.Render()
	}

	if printStats {
		cpDir, err := os.Open(checkpointDirectory)
		if err != nil {
			return err
		}
		defer cpDir.Close()

		// Get dump statistics with crit
		dumpStatistics, err := crit.GetDumpStats(cpDir.Name())
		if err != nil {
			return fmt.Errorf("unable to display checkpointing statistics: %w", err)
		}

		table = tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{
			"Freezing Time",
			"Frozen Time",
			"Memdump Time",
			"Memwrite Time",
			"Pages Scanned",
			"Pages Written",
		})
		table.Append([]string{
			fmt.Sprintf("%d us", dumpStatistics.GetFreezingTime()),
			fmt.Sprintf("%d us", dumpStatistics.GetFrozenTime()),
			fmt.Sprintf("%d us", dumpStatistics.GetMemdumpTime()),
			fmt.Sprintf("%d us", dumpStatistics.GetMemwriteTime()),
			fmt.Sprintf("%d", dumpStatistics.GetPagesScanned()),
			fmt.Sprintf("%d", dumpStatistics.GetPagesWritten()),
		})
		fmt.Println("\nCRIU dump statistics")
		table.Render()
	}

	return nil
}

func dirSize(path string) (size int64, err error) {
	err = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}

		return err
	})

	return size, err
}

func getCheckpointSize(path string) (size int64, err error) {
	dir := filepath.Join(path, metadata.CheckpointDirectory)

	return dirSize(dir)
}

func shortenPath(path string) string {
	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) <= 2 {
		return path
	}
	return filepath.Join("..", filepath.Join(parts[len(parts)-2:]...))
}
