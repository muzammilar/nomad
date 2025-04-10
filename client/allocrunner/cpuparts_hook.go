// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package allocrunner

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/lib/cgroupslib"
	"github.com/hashicorp/nomad/client/lib/idset"
	"github.com/hashicorp/nomad/client/lib/numalib/hw"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	cpuPartsHookName = "cpuparts_hook"
)

// cpuPartsHooks is responsible for managing cpuset partitioning on Linux
// nodes. This mechanism works by segregating tasks that make use of "cpu" vs.
// "cores" resources. Tasks that make use of "cpu" resource actually make use
// of shared cores that have not been reserved. The scheduler ensures enough
// cores on a node are not reserved such that all tasks have the minimum amount
// of cpu bandwidth they requested.
type cpuPartsHook struct {
	logger  hclog.Logger
	allocID string

	reservations *idset.Set[hw.CoreID]
	partitions   cgroupslib.Partition
}

func newCPUPartsHook(
	logger hclog.Logger,
	partitions cgroupslib.Partition,
	alloc *structs.Allocation,
) *cpuPartsHook {
	return &cpuPartsHook{
		logger:       logger.Named(cpuPartsHookName),
		allocID:      alloc.ID,
		partitions:   partitions,
		reservations: alloc.ReservedCores(),
	}
}

func (h *cpuPartsHook) Name() string {
	return cpuPartsHookName
}

// statically assert the hook implements the expected interfaces
var (
	_ interfaces.RunnerPrerunHook  = (*cpuPartsHook)(nil)
	_ interfaces.RunnerPostrunHook = (*cpuPartsHook)(nil)
)

func (h *cpuPartsHook) Prerun(_ *taskenv.TaskEnv) error {
	return h.partitions.Reserve(h.reservations)
}

func (h *cpuPartsHook) Postrun() error {
	return h.partitions.Release(h.reservations)
}
