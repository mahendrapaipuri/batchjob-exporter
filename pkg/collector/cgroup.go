package collector

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
)

const (
	// Max cgroup subsystems count that is used from BPF side
	// to define a max index for the default controllers on tasks.
	// For further documentation check BPF part.
	cgroupSubSysCount = 15
	genericSubsystem  = "compute"
)

// Resource Managers.
const (
	slurm   = "slurm"
	libvirt = "libvirt"
)

// Block IO Op names.
const (
	readOp  = "Read"
	writeOp = "Write"
)

// Regular expressions of cgroup paths for different resource managers.
/*
	For v1 possibilities are /cpuacct/slurm/uid_1000/job_211
							 /memory/slurm/uid_1000/job_211

	For v2 possibilities are /system.slice/slurmstepd.scope/job_211
							/system.slice/slurmstepd.scope/job_211/step_interactive
							/system.slice/slurmstepd.scope/job_211/step_extern/user/task_0
*/
var (
	slurmCgroupPathRegex  = regexp.MustCompile("^.*/slurm(?:.*?)/job_([0-9]+)(?:.*$)")
	slurmIgnoreProcsRegex = regexp.MustCompile("slurmstepd:(.*)|sleep ([0-9]+)|/bin/bash (.*)/slurm_script")
)

// Ref: https://libvirt.org/cgroups.html#legacy-cgroups-layout
// Take escaped unicode characters in regex
/*
	For v1 possibilities are /cpuacct/machine.slice/machine-qemu\x2d2\x2dinstance\x2d00000001.scope
							 /memory/machine.slice/machine-qemu\x2d2\x2dinstance\x2d00000001.scope

	For v2 possibilities are /machine.slice/machine-qemu\x2d2\x2dinstance\x2d00000001.scope
							 /machine.slice/machine-qemu\x2d2\x2dinstance\x2d00000001.scope/libvirt
*/
var (
	libvirtCgroupPathRegex = regexp.MustCompile("^.*/(?:.+?)-qemu-(?:[0-9]+)-(instance-[0-9a-f]+)(?:.*$)")
)

// CLI options.
var (
	activeController = CEEMSExporterApp.Flag(
		"collector.cgroup.active-subsystem",
		"Active cgroup subsystem for cgroups v1.",
	).Default("cpuacct").String()

	// Hidden opts for e2e and unit tests.
	forceCgroupsVersion = CEEMSExporterApp.Flag(
		"collector.cgroups.force-version",
		"Set cgroups version manually. Used only for testing.",
	).Hidden().Enum("v1", "v2")
)

type cgroupPath struct {
	abs, rel string
}

// String implements stringer interface of the struct.
func (c *cgroupPath) String() string {
	return c.abs
}

type cgroup struct {
	id       string
	uuid     string // uuid is the identifier known to user whereas id is identifier used by resource manager internally
	procs    []procfs.Proc
	path     cgroupPath
	children []cgroupPath // All the children under this root cgroup
}

// String implements stringer interface of the struct.
func (c *cgroup) String() string {
	return fmt.Sprintf(
		"id: %s path: %s num_procs: %d num_children: %d",
		c.id,
		c.path,
		len(c.procs),
		len(c.children),
	)
}

// cgroupManager is the container that have cgroup information of resource manager.
type cgroupManager struct {
	logger           *slog.Logger
	fs               procfs.FS
	mode             cgroups.CGMode    // cgroups mode: unified, legacy, hybrid
	root             string            // cgroups root
	slice            string            // Slice under which cgroups are managed eg system.slice, machine.slice
	scope            string            // Scope under which cgroups are managed eg slurmstepd.scope, machine-qemu\x2d1\x2dvm1.scope
	activeController string            // Active controller for cgroups v1
	mountPoint       string            // Path under which resource manager creates cgroups
	manager          string            // cgroup manager
	idRegex          *regexp.Regexp    // Regular expression to capture cgroup ID set by resource manager
	isChild          func(string) bool // Function to identify child cgroup paths. Function must return true if cgroup is a child to root cgroup
	ignoreProc       func(string) bool // Function to filter processes in cgroup based on cmdline. Function must return true if process must be ignored
}

// String implements stringer interface of the struct.
func (c *cgroupManager) String() string {
	return fmt.Sprintf(
		"mode: %d root: %s slice: %s scope: %s mount: %s manager: %s",
		c.mode,
		c.root,
		c.slice,
		c.scope,
		c.mountPoint,
		c.manager,
	)
}

// setMountPoint sets mountPoint for thc cgroupManager struct.
func (c *cgroupManager) setMountPoint() {
	switch c.manager {
	case slurm:
		switch c.mode { //nolint:exhaustive
		case cgroups.Unified:
			// /sys/fs/cgroup/system.slice/slurmstepd.scope
			c.mountPoint = filepath.Join(c.root, c.slice, c.scope)
		default:
			// /sys/fs/cgroup/cpuacct/slurm
			c.mountPoint = filepath.Join(c.root, c.activeController, c.manager)

			// For cgroups v1 we need to shift root to /sys/fs/cgroup/cpuacct
			c.root = filepath.Join(c.root, c.activeController)
		}
	case libvirt:
		switch c.mode { //nolint:exhaustive
		case cgroups.Unified:
			// /sys/fs/cgroup/machine.slice
			c.mountPoint = filepath.Join(c.root, c.slice)
		default:
			// /sys/fs/cgroup/cpuacct/machine.slice
			c.mountPoint = filepath.Join(c.root, c.activeController, c.slice)

			// For cgroups v1 we need to shift root to /sys/fs/cgroup/cpuacct
			c.root = filepath.Join(c.root, c.activeController)
		}
	default:
		c.mountPoint = c.root
	}
}

// discover finds all the active cgroups in the given mountpoint.
func (c *cgroupManager) discover() ([]cgroup, error) {
	var cgroups []cgroup

	cgroupProcs := make(map[string][]procfs.Proc)

	cgroupChildren := make(map[string][]cgroupPath)

	// Walk through all cgroups and get cgroup paths
	// https://goplay.tools/snippet/coVDkIozuhg
	if err := filepath.WalkDir(c.mountPoint, func(p string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Ignore paths that are not directories
		if !info.IsDir() {
			return nil
		}

		// Get relative path of cgroup
		rel, err := filepath.Rel(c.root, p)
		if err != nil {
			c.logger.Error("Failed to resolve relative path for cgroup", "path", p, "err", err)

			return nil
		}

		// Unescape UTF-8 characters in cgroup path
		sanitizedPath, err := unescapeString(p)
		if err != nil {
			c.logger.Error("Failed to sanitize cgroup path", "path", p, "err", err)

			return nil
		}

		// Get cgroup ID which is instance ID
		cgroupIDMatches := c.idRegex.FindStringSubmatch(sanitizedPath)
		if len(cgroupIDMatches) <= 1 {
			return nil
		}

		id := strings.TrimSpace(cgroupIDMatches[1])
		if id == "" {
			c.logger.Error("Empty cgroup ID", "path", p)

			return nil
		}

		// Find procs in this cgroup
		if data, err := os.ReadFile(filepath.Join(p, "cgroup.procs")); err == nil {
			scanner := bufio.NewScanner(bytes.NewReader(data))
			for scanner.Scan() {
				if pid, err := strconv.ParseInt(scanner.Text(), 10, 0); err == nil {
					if proc, err := c.fs.Proc(int(pid)); err == nil {
						cgroupProcs[id] = append(cgroupProcs[id], proc)
					}
				}
			}
		}

		// Ignore child cgroups. We are only interested in root cgroup
		if c.isChild(p) {
			cgroupChildren[id] = append(cgroupChildren[id], cgroupPath{abs: sanitizedPath, rel: rel})

			return nil
		}

		// By default set id and uuid to same cgroup ID and if the resource
		// manager has two representations, override it in corresponding
		// collector. For instance, it applies only to libvirt
		cgrp := cgroup{
			id:   id,
			uuid: id,
			path: cgroupPath{abs: sanitizedPath, rel: rel},
		}

		cgroups = append(cgroups, cgrp)
		cgroupChildren[id] = append(cgroupChildren[id], cgroupPath{abs: sanitizedPath, rel: rel})

		return nil
	}); err != nil {
		c.logger.Error("Error walking cgroup subsystem", "path", c.mountPoint, "err", err)

		return nil, err
	}

	// Merge cgroupProcs and cgroupChildren with cgroups slice
	for icgrp := range cgroups {
		if procs, ok := cgroupProcs[cgroups[icgrp].id]; ok {
			cgroups[icgrp].procs = procs
		}

		if children, ok := cgroupChildren[cgroups[icgrp].id]; ok {
			cgroups[icgrp].children = children
		}
	}

	return cgroups, nil
}

// NewCgroupManager returns an instance of cgroupManager based on resource manager.
func NewCgroupManager(name string, logger *slog.Logger) (*cgroupManager, error) {
	// Instantiate a new Proc FS
	fs, err := procfs.NewFS(*procfsPath)
	if err != nil {
		logger.Error("Unable to open procfs", "path", *procfsPath, "err", err)

		return nil, err
	}

	var manager *cgroupManager

	switch name {
	case slurm:
		if (*forceCgroupsVersion == "" && cgroups.Mode() == cgroups.Unified) || *forceCgroupsVersion == "v2" {
			manager = &cgroupManager{
				logger: logger,
				fs:     fs,
				mode:   cgroups.Unified,
				root:   *cgroupfsPath,
				slice:  "system.slice",
				scope:  "slurmstepd.scope",
			}
		} else {
			var mode cgroups.CGMode
			if *forceCgroupsVersion == "v1" {
				mode = cgroups.Legacy
			} else {
				mode = cgroups.Mode()
			}

			manager = &cgroupManager{
				logger:           logger,
				fs:               fs,
				mode:             mode,
				root:             *cgroupfsPath,
				activeController: *activeController,
				slice:            slurm,
			}
		}

		// Add manager field
		manager.manager = slurm

		// Add path regex
		manager.idRegex = slurmCgroupPathRegex

		// Identify child cgroup
		manager.isChild = func(p string) bool {
			return strings.Contains(p, "/step_")
		}
		manager.ignoreProc = func(p string) bool {
			return slurmIgnoreProcsRegex.MatchString(p)
		}

		// Set mountpoint
		manager.setMountPoint()

		return manager, nil

	case libvirt:
		if (*forceCgroupsVersion == "" && cgroups.Mode() == cgroups.Unified) || *forceCgroupsVersion == "v2" {
			manager = &cgroupManager{
				logger: logger,
				fs:     fs,
				mode:   cgroups.Unified,
				root:   *cgroupfsPath,
				slice:  "machine.slice",
			}
		} else {
			var mode cgroups.CGMode
			if *forceCgroupsVersion == "v1" {
				mode = cgroups.Legacy
			} else {
				mode = cgroups.Mode()
			}

			manager = &cgroupManager{
				logger:           logger,
				fs:               fs,
				mode:             mode,
				root:             *cgroupfsPath,
				activeController: *activeController,
				slice:            "machine.slice",
			}
		}

		// Add manager field
		manager.manager = libvirt

		// Add path regex
		manager.idRegex = libvirtCgroupPathRegex

		// Identify child cgroup
		// In cgroups v1, all the child cgroups like emulator, vcpu* are flat whereas
		// in v2 they are all inside libvirt child
		manager.isChild = func(p string) bool {
			return strings.Contains(p, "/libvirt") || strings.Contains(p, "/emulator") || strings.Contains(p, "/vcpu")
		}
		manager.ignoreProc = func(p string) bool {
			return false
		}

		// Set mountpoint
		manager.setMountPoint()

		return manager, nil

	default:
		return nil, errors.New("unknown resource manager")
	}
}

// cgMetric contains metrics returned by cgroup.
type cgMetric struct {
	path            string
	cpuUser         float64
	cpuSystem       float64
	cpuTotal        float64
	cpus            int
	cpuPressure     float64
	memoryRSS       float64
	memoryCache     float64
	memoryUsed      float64
	memoryTotal     float64
	memoryFailCount float64
	memswUsed       float64
	memswTotal      float64
	memswFailCount  float64
	memoryPressure  float64
	blkioReadBytes  map[string]float64
	blkioWriteBytes map[string]float64
	blkioReadReqs   map[string]float64
	blkioWriteReqs  map[string]float64
	blkioPressure   float64
	rdmaHCAHandles  map[string]float64
	rdmaHCAObjects  map[string]float64
	uuid            string
	err             bool
}

// cgroupCollector collects cgroup metrics for different resource managers.
type cgroupCollector struct {
	logger            *slog.Logger
	cgroupManager     *cgroupManager
	opts              cgroupOpts
	hostname          string
	hostMemInfo       map[string]float64
	blockDevices      map[string]string
	numCgs            *prometheus.Desc
	cgCPUUser         *prometheus.Desc
	cgCPUSystem       *prometheus.Desc
	cgCPUs            *prometheus.Desc
	cgCPUPressure     *prometheus.Desc
	cgMemoryRSS       *prometheus.Desc
	cgMemoryCache     *prometheus.Desc
	cgMemoryUsed      *prometheus.Desc
	cgMemoryTotal     *prometheus.Desc
	cgMemoryFailCount *prometheus.Desc
	cgMemswUsed       *prometheus.Desc
	cgMemswTotal      *prometheus.Desc
	cgMemswFailCount  *prometheus.Desc
	cgMemoryPressure  *prometheus.Desc
	cgBlkioReadBytes  *prometheus.Desc
	cgBlkioWriteBytes *prometheus.Desc
	cgBlkioReadReqs   *prometheus.Desc
	cgBlkioWriteReqs  *prometheus.Desc
	cgBlkioPressure   *prometheus.Desc
	cgRDMAHCAHandles  *prometheus.Desc
	cgRDMAHCAObjects  *prometheus.Desc
	collectError      *prometheus.Desc
}

type cgroupOpts struct {
	collectSwapMemStats bool
	collectBlockIOStats bool
	collectPSIStats     bool
}

// NewCgroupCollector returns a new cgroupCollector exposing a summary of cgroups.
func NewCgroupCollector(logger *slog.Logger, cgManager *cgroupManager, opts cgroupOpts) (*cgroupCollector, error) {
	// Get total memory of host
	hostMemInfo := make(map[string]float64)

	file, err := os.Open(procFilePath("meminfo"))
	if err == nil {
		if memInfo, err := parseMemInfo(file); err == nil {
			hostMemInfo = memInfo
		}
	} else {
		logger.Error("Failed to get total memory of the host", "err", err)
	}

	defer file.Close()

	// Read block IO stats just to get block devices info.
	// We construct a map from major:minor to device name using this info
	blockDevices := make(map[string]string)

	if blockdevice, err := blockdevice.NewFS(*procfsPath, *sysPath); err == nil {
		if stats, err := blockdevice.ProcDiskstats(); err == nil {
			for _, s := range stats {
				blockDevices[fmt.Sprintf("%d:%d", s.Info.MajorNumber, s.Info.MinorNumber)] = s.Info.DeviceName
			}
		} else {
			logger.Error("Failed to get stats of block devices on the host", "err", err)
		}
	} else {
		logger.Error("Failed to get list of block devices on the host", "err", err)
	}

	return &cgroupCollector{
		logger:        logger,
		cgroupManager: cgManager,
		opts:          opts,
		hostMemInfo:   hostMemInfo,
		hostname:      hostname,
		blockDevices:  blockDevices,
		numCgs: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "units"),
			"Total number of jobs",
			[]string{"manager", "hostname"},
			nil,
		),
		cgCPUUser: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_cpu_user_seconds_total"),
			"Total job CPU user seconds",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgCPUSystem: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_cpu_system_seconds_total"),
			"Total job CPU system seconds",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgCPUs: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_cpus"),
			"Total number of job CPUs",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgCPUPressure: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_cpu_psi_seconds"),
			"Total CPU PSI in seconds",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemoryRSS: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memory_rss_bytes"),
			"Memory RSS used in bytes",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemoryCache: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memory_cache_bytes"),
			"Memory cache used in bytes",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemoryUsed: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memory_used_bytes"),
			"Memory used in bytes",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemoryTotal: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memory_total_bytes"),
			"Memory total in bytes",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemoryFailCount: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memory_fail_count"),
			"Memory fail count",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemswUsed: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memsw_used_bytes"),
			"Swap used in bytes",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemswTotal: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memsw_total_bytes"),
			"Swap total in bytes",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemswFailCount: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memsw_fail_count"),
			"Swap fail count",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgMemoryPressure: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_memory_psi_seconds"),
			"Total memory PSI in seconds",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
		cgBlkioReadBytes: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_blkio_read_total_bytes"),
			"Total block IO read bytes",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		cgBlkioWriteBytes: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_blkio_write_total_bytes"),
			"Total block IO write bytes",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		cgBlkioReadReqs: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_blkio_read_total_requests"),
			"Total block IO read requests",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		cgBlkioWriteReqs: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_blkio_write_total_requests"),
			"Total block IO write requests",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		cgBlkioPressure: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_blkio_psi_seconds"),
			"Total block IO PSI in seconds",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		cgRDMAHCAHandles: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_rdma_hca_handles"),
			"Current number of RDMA HCA handles",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		cgRDMAHCAObjects: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "unit_rdma_hca_objects"),
			"Current number of RDMA HCA objects",
			[]string{"manager", "hostname", "uuid", "device"},
			nil,
		),
		collectError: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, genericSubsystem, "collect_error"),
			"Indicates collection error, 0=no error, 1=error",
			[]string{"manager", "hostname", "uuid"},
			nil,
		),
	}, nil
}

// Update updates cgroup metrics on given channel.
func (c *cgroupCollector) Update(ch chan<- prometheus.Metric, metrics []cgMetric) error {
	// Fetch metrics
	metrics = c.doUpdate(metrics)

	// First send num jobs on the current host
	ch <- prometheus.MustNewConstMetric(c.numCgs, prometheus.GaugeValue, float64(len(metrics)), c.cgroupManager.manager, c.hostname)

	// Send metrics of each cgroup
	for _, m := range metrics {
		if m.err {
			ch <- prometheus.MustNewConstMetric(c.collectError, prometheus.GaugeValue, float64(1), c.cgroupManager.manager, c.hostname, m.uuid)
		}

		// CPU stats
		ch <- prometheus.MustNewConstMetric(c.cgCPUUser, prometheus.CounterValue, m.cpuUser, c.cgroupManager.manager, c.hostname, m.uuid)
		ch <- prometheus.MustNewConstMetric(c.cgCPUSystem, prometheus.CounterValue, m.cpuSystem, c.cgroupManager.manager, c.hostname, m.uuid)
		ch <- prometheus.MustNewConstMetric(c.cgCPUs, prometheus.GaugeValue, float64(m.cpus), c.cgroupManager.manager, c.hostname, m.uuid)

		// Memory stats
		ch <- prometheus.MustNewConstMetric(c.cgMemoryRSS, prometheus.GaugeValue, m.memoryRSS, c.cgroupManager.manager, c.hostname, m.uuid)
		ch <- prometheus.MustNewConstMetric(c.cgMemoryCache, prometheus.GaugeValue, m.memoryCache, c.cgroupManager.manager, c.hostname, m.uuid)
		ch <- prometheus.MustNewConstMetric(c.cgMemoryUsed, prometheus.GaugeValue, m.memoryUsed, c.cgroupManager.manager, c.hostname, m.uuid)
		ch <- prometheus.MustNewConstMetric(c.cgMemoryTotal, prometheus.GaugeValue, m.memoryTotal, c.cgroupManager.manager, c.hostname, m.uuid)
		ch <- prometheus.MustNewConstMetric(c.cgMemoryFailCount, prometheus.GaugeValue, m.memoryFailCount, c.cgroupManager.manager, c.hostname, m.uuid)

		// Memory swap stats
		if c.opts.collectSwapMemStats {
			ch <- prometheus.MustNewConstMetric(c.cgMemswUsed, prometheus.GaugeValue, m.memswUsed, c.cgroupManager.manager, c.hostname, m.uuid)
			ch <- prometheus.MustNewConstMetric(c.cgMemswTotal, prometheus.GaugeValue, m.memswTotal, c.cgroupManager.manager, c.hostname, m.uuid)
			ch <- prometheus.MustNewConstMetric(c.cgMemswFailCount, prometheus.GaugeValue, m.memswFailCount, c.cgroupManager.manager, c.hostname, m.uuid)
		}

		// Block IO stats
		if c.opts.collectBlockIOStats {
			for device := range m.blkioReadBytes {
				if v, ok := m.blkioReadBytes[device]; ok && v > 0 {
					ch <- prometheus.MustNewConstMetric(c.cgBlkioReadBytes, prometheus.GaugeValue, v, c.cgroupManager.manager, c.hostname, m.uuid, device)
				}

				if v, ok := m.blkioWriteBytes[device]; ok && v > 0 {
					ch <- prometheus.MustNewConstMetric(c.cgBlkioWriteBytes, prometheus.GaugeValue, v, c.cgroupManager.manager, c.hostname, m.uuid, device)
				}

				if v, ok := m.blkioReadReqs[device]; ok && v > 0 {
					ch <- prometheus.MustNewConstMetric(c.cgBlkioReadReqs, prometheus.GaugeValue, v, c.cgroupManager.manager, c.hostname, m.uuid, device)
				}

				if v, ok := m.blkioWriteReqs[device]; ok && v > 0 {
					ch <- prometheus.MustNewConstMetric(c.cgBlkioWriteReqs, prometheus.GaugeValue, v, c.cgroupManager.manager, c.hostname, m.uuid, device)
				}
			}
		}

		// PSI stats
		if c.opts.collectPSIStats {
			ch <- prometheus.MustNewConstMetric(c.cgCPUPressure, prometheus.GaugeValue, m.cpuPressure, c.cgroupManager.manager, c.hostname, m.uuid)
			ch <- prometheus.MustNewConstMetric(c.cgMemoryPressure, prometheus.GaugeValue, m.memoryPressure, c.cgroupManager.manager, c.hostname, m.uuid)
		}

		// RDMA stats
		for device, handles := range m.rdmaHCAHandles {
			if handles > 0 {
				ch <- prometheus.MustNewConstMetric(c.cgRDMAHCAHandles, prometheus.GaugeValue, handles, c.cgroupManager.manager, c.hostname, m.uuid, device)
			}
		}

		for device, objects := range m.rdmaHCAHandles {
			if objects > 0 {
				ch <- prometheus.MustNewConstMetric(c.cgRDMAHCAObjects, prometheus.GaugeValue, objects, c.cgroupManager.manager, c.hostname, m.uuid, device)
			}
		}
	}

	return nil
}

// Stop releases any system resources held by collector.
func (c *cgroupCollector) Stop(_ context.Context) error {
	return nil
}

// doUpdate gets metrics of current active cgroups.
func (c *cgroupCollector) doUpdate(metrics []cgMetric) []cgMetric {
	// Start wait group for go routines
	wg := &sync.WaitGroup{}
	wg.Add(len(metrics))

	// No need for any lock primitives here as we read/write
	// a different element of slice in each go routine
	for i := range metrics {
		go func(idx int) {
			defer wg.Done()

			c.update(&metrics[idx])
		}(i)
	}

	// Wait for all go routines
	wg.Wait()

	return metrics
}

// update get metrics of a given cgroup path.
func (c *cgroupCollector) update(m *cgMetric) {
	if c.cgroupManager.mode == cgroups.Unified {
		c.statsV2(m)
	} else {
		c.statsV1(m)
	}
}

// parseCPUSet parses cpuset.cpus file to return a list of CPUs in the cgroup.
func (c *cgroupCollector) parseCPUSet(cpuset string) ([]string, error) {
	var cpus []string

	var start, end int

	var err error

	if cpuset == "" {
		return nil, errors.New("empty cpuset file")
	}

	ranges := strings.Split(cpuset, ",")
	for _, r := range ranges {
		boundaries := strings.Split(r, "-")
		if len(boundaries) == 1 {
			start, err = strconv.Atoi(boundaries[0])
			if err != nil {
				return nil, err
			}

			end = start
		} else if len(boundaries) == 2 {
			start, err = strconv.Atoi(boundaries[0])
			if err != nil {
				return nil, err
			}

			end, err = strconv.Atoi(boundaries[1])
			if err != nil {
				return nil, err
			}
		}

		for e := start; e <= end; e++ {
			cpu := strconv.Itoa(e)
			cpus = append(cpus, cpu)
		}
	}

	return cpus, nil
}

// getCPUs returns list of CPUs in the cgroup.
func (c *cgroupCollector) getCPUs(path string) ([]string, error) {
	var cpusPath string
	if c.cgroupManager.mode == cgroups.Unified {
		cpusPath = fmt.Sprintf("%s%s/cpuset.cpus.effective", *cgroupfsPath, path)
	} else {
		cpusPath = fmt.Sprintf("%s/cpuset%s/cpuset.cpus", *cgroupfsPath, path)
	}

	if !fileExists(cpusPath) {
		return nil, fmt.Errorf("cpuset file %s not found", cpusPath)
	}

	cpusData, err := os.ReadFile(cpusPath)
	if err != nil {
		c.logger.Error("Error reading cpuset", "cpuset", cpusPath, "err", err)

		return nil, err
	}

	cpus, err := c.parseCPUSet(strings.TrimSuffix(string(cpusData), "\n"))
	if err != nil {
		c.logger.Error("Error parsing cpuset", "cpuset", cpusPath, "err", err)

		return nil, err
	}

	return cpus, nil
}

// statsV1 fetches metrics from cgroups v1.
func (c *cgroupCollector) statsV1(metric *cgMetric) {
	path := metric.path

	c.logger.Debug("Loading cgroup v1", "path", path)

	ctrl, err := cgroup1.Load(cgroup1.StaticPath(path), cgroup1.WithHierarchy(subsystem))
	if err != nil {
		metric.err = true

		c.logger.Error("Failed to load cgroups", "path", path, "err", err)

		return
	}

	// Load cgroup stats
	stats, err := ctrl.Stat(cgroup1.IgnoreNotExist)
	if err != nil {
		metric.err = true

		c.logger.Error("Failed to stat cgroups", "path", path, "err", err)

		return
	}

	if stats == nil {
		metric.err = true

		c.logger.Error("Cgroup stats are nil", "path", path)

		return
	}

	// Get CPU stats
	if stats.GetCPU() != nil {
		if stats.GetCPU().GetUsage() != nil {
			metric.cpuUser = float64(stats.GetCPU().GetUsage().GetUser()) / 1000000000.0
			metric.cpuSystem = float64(stats.GetCPU().GetUsage().GetKernel()) / 1000000000.0
			metric.cpuTotal = float64(stats.GetCPU().GetUsage().GetTotal()) / 1000000000.0
		}
	}

	if cpus, err := c.getCPUs(path); err == nil {
		metric.cpus = len(cpus)
	}

	// Get memory stats
	if stats.GetMemory() != nil {
		metric.memoryRSS = float64(stats.GetMemory().GetTotalRSS())
		metric.memoryCache = float64(stats.GetMemory().GetTotalCache())

		if stats.GetMemory().GetUsage() != nil {
			metric.memoryUsed = float64(stats.GetMemory().GetUsage().GetUsage())
			// If memory usage limit is set as "max", cgroups lib will set it to
			// math.MaxUint64. Here we replace it with total system memory
			if stats.GetMemory().GetUsage().GetLimit() == math.MaxUint64 && c.hostMemInfo["MemTotal_bytes"] > 0 {
				metric.memoryTotal = c.hostMemInfo["MemTotal_bytes"]
			} else {
				metric.memoryTotal = float64(stats.GetMemory().GetUsage().GetLimit())
			}

			metric.memoryFailCount = float64(stats.GetMemory().GetUsage().GetFailcnt())
		}

		if stats.GetMemory().GetSwap() != nil {
			metric.memswUsed = float64(stats.GetMemory().GetSwap().GetUsage())
			// If memory usage limit is set as "max", cgroups lib will set it to
			// math.MaxUint64. Here we replace it with total system memory
			if stats.GetMemory().GetSwap().GetLimit() == math.MaxUint64 {
				switch {
				case c.hostMemInfo["SwapTotal_bytes"] > 0:
					metric.memswTotal = c.hostMemInfo["SwapTotal_bytes"]
				case c.hostMemInfo["MemTotal_bytes"] > 0:
					metric.memswTotal = c.hostMemInfo["MemTotal_bytes"]
				default:
					metric.memswTotal = float64(stats.GetMemory().GetSwap().GetLimit())
				}
			} else {
				metric.memswTotal = float64(stats.GetMemory().GetSwap().GetLimit())
			}

			metric.memswFailCount = float64(stats.GetMemory().GetSwap().GetFailcnt())
		}
	}

	// Get block IO stats
	if stats.GetBlkio() != nil {
		metric.blkioReadBytes = make(map[string]float64)
		metric.blkioReadReqs = make(map[string]float64)
		metric.blkioWriteBytes = make(map[string]float64)
		metric.blkioWriteReqs = make(map[string]float64)

		for _, stat := range stats.GetBlkio().GetIoServiceBytesRecursive() {
			devName := c.blockDevices[fmt.Sprintf("%d:%d", stat.GetMajor(), stat.GetMinor())]

			if stat.GetOp() == readOp {
				metric.blkioReadBytes[devName] = float64(stat.GetValue())
			} else if stat.GetOp() == writeOp {
				metric.blkioWriteBytes[devName] = float64(stat.GetValue())
			}
		}

		for _, stat := range stats.GetBlkio().GetIoServicedRecursive() {
			devName := c.blockDevices[fmt.Sprintf("%d:%d", stat.GetMajor(), stat.GetMinor())]

			if stat.GetOp() == readOp {
				metric.blkioReadReqs[devName] = float64(stat.GetValue())
			} else if stat.GetOp() == writeOp {
				metric.blkioWriteReqs[devName] = float64(stat.GetValue())
			}
		}
	}

	// Get RDMA metrics if available
	if stats.GetRdma() != nil {
		metric.rdmaHCAHandles = make(map[string]float64)
		metric.rdmaHCAObjects = make(map[string]float64)

		for _, device := range stats.GetRdma().GetCurrent() {
			metric.rdmaHCAHandles[device.GetDevice()] = float64(device.GetHcaHandles())
			metric.rdmaHCAObjects[device.GetDevice()] = float64(device.GetHcaObjects())
		}
	}
}

// statsV2 fetches metrics from cgroups v2.
func (c *cgroupCollector) statsV2(metric *cgMetric) {
	path := metric.path

	c.logger.Debug("Loading cgroup v2", "path", path)

	// Load cgroups
	ctrl, err := cgroup2.Load(path, cgroup2.WithMountpoint(*cgroupfsPath))
	if err != nil {
		metric.err = true

		c.logger.Error("Failed to load cgroups", "path", path, "err", err)

		return
	}

	// Get stats from cgroup
	stats, err := ctrl.Stat()
	if err != nil {
		metric.err = true

		c.logger.Error("Failed to stat cgroups", "path", path, "err", err)

		return
	}

	if stats == nil {
		metric.err = true

		c.logger.Error("Cgroup stats are nil", "path", path)

		return
	}

	// Get CPU stats
	if stats.GetCPU() != nil {
		metric.cpuUser = float64(stats.GetCPU().GetUserUsec()) / 1000000.0
		metric.cpuSystem = float64(stats.GetCPU().GetSystemUsec()) / 1000000.0
		metric.cpuTotal = float64(stats.GetCPU().GetUsageUsec()) / 1000000.0

		if stats.GetCPU().GetPSI() != nil {
			metric.cpuPressure = float64(stats.GetCPU().GetPSI().GetFull().GetTotal()) / 1000000.0
		}
	}

	if cpus, err := c.getCPUs(path); err == nil {
		metric.cpus = len(cpus)
	}

	// Get memory stats
	// cgroups2 does not expose swap memory events. So we dont set memswFailCount
	if stats.GetMemory() != nil {
		metric.memoryUsed = float64(stats.GetMemory().GetUsage())
		// If memory usage limit is set as "max", cgroups lib will set it to
		// math.MaxUint64. Here we replace it with total system memory
		if stats.GetMemory().GetUsageLimit() == math.MaxUint64 && c.hostMemInfo["MemTotal_bytes"] > 0 {
			metric.memoryTotal = c.hostMemInfo["MemTotal_bytes"]
		} else {
			metric.memoryTotal = float64(stats.GetMemory().GetUsageLimit())
		}

		metric.memoryCache = float64(stats.GetMemory().GetFile()) // This is page cache
		metric.memoryRSS = float64(stats.GetMemory().GetAnon())
		metric.memswUsed = float64(stats.GetMemory().GetSwapUsage())
		// If memory usage limit is set as "max", cgroups lib will set it to
		// math.MaxUint64. Here we replace it with either total swap/system memory
		if stats.GetMemory().GetSwapLimit() == math.MaxUint64 {
			switch {
			case c.hostMemInfo["SwapTotal_bytes"] > 0:
				metric.memswTotal = c.hostMemInfo["SwapTotal_bytes"]
			case c.hostMemInfo["MemTotal_bytes"] > 0:
				metric.memswTotal = c.hostMemInfo["MemTotal_bytes"]
			default:
				metric.memswTotal = float64(stats.GetMemory().GetSwapLimit())
			}
		} else {
			metric.memswTotal = float64(stats.GetMemory().GetSwapLimit())
		}

		if stats.GetMemory().GetPSI() != nil {
			metric.memoryPressure = float64(stats.GetMemory().GetPSI().GetFull().GetTotal()) / 1000000.0
		}
	}
	// Get memory events
	if stats.GetMemoryEvents() != nil {
		metric.memoryFailCount = float64(stats.GetMemoryEvents().GetOom())
	}

	// Get block IO stats
	if stats.GetIo() != nil {
		metric.blkioReadBytes = make(map[string]float64)
		metric.blkioReadReqs = make(map[string]float64)
		metric.blkioWriteBytes = make(map[string]float64)
		metric.blkioWriteReqs = make(map[string]float64)

		for _, stat := range stats.GetIo().GetUsage() {
			devName := c.blockDevices[fmt.Sprintf("%d:%d", stat.GetMajor(), stat.GetMinor())]
			metric.blkioReadBytes[devName] = float64(stat.GetRbytes())
			metric.blkioReadReqs[devName] = float64(stat.GetRios())
			metric.blkioWriteBytes[devName] = float64(stat.GetWbytes())
			metric.blkioWriteReqs[devName] = float64(stat.GetWios())
		}

		if stats.GetIo().GetPSI() != nil {
			metric.blkioPressure = float64(stats.GetIo().GetPSI().GetFull().GetTotal()) / 1000000.0
		}
	}

	// Get RDMA stats
	if stats.GetRdma() != nil {
		metric.rdmaHCAHandles = make(map[string]float64)
		metric.rdmaHCAObjects = make(map[string]float64)

		for _, device := range stats.GetRdma().GetCurrent() {
			metric.rdmaHCAHandles[device.GetDevice()] = float64(device.GetHcaHandles())
			metric.rdmaHCAObjects[device.GetDevice()] = float64(device.GetHcaObjects())
		}
	}
}

// subsystem returns cgroups v1 subsystems.
func subsystem() ([]cgroup1.Subsystem, error) {
	s := []cgroup1.Subsystem{
		cgroup1.NewCpuacct(*cgroupfsPath),
		cgroup1.NewMemory(*cgroupfsPath),
		cgroup1.NewRdma(*cgroupfsPath),
		cgroup1.NewPids(*cgroupfsPath),
		cgroup1.NewBlkio(*cgroupfsPath),
		cgroup1.NewCpuset(*cgroupfsPath),
	}

	return s, nil
}

// cgroupController is a container for cgroup controllers in v1.
type cgroupController struct {
	id     uint64 // Hierarchy unique ID
	idx    uint64 // Cgroup SubSys index
	name   string // Controller name
	active bool   // Will be set to true if controller is set and active
}

// parseCgroupSubSysIds returns cgroup controllers for cgroups v1.
func parseCgroupSubSysIds() ([]cgroupController, error) {
	var cgroupControllers []cgroupController

	// Read /proc/cgroups file
	file, err := os.Open(procFilePath("cgroups"))
	if err != nil {
		return nil, err
	}

	defer file.Close()

	fscanner := bufio.NewScanner(file)

	var idx uint64 = 0

	fscanner.Scan() // ignore first entry

	for fscanner.Scan() {
		line := fscanner.Text()
		fields := strings.Fields(line)

		/* We care only for the controllers that we want */
		if idx >= cgroupSubSysCount {
			/* Maybe some cgroups are not upstream? */
			return cgroupControllers, fmt.Errorf(
				"cgroup default subsystem '%s' is indexed at idx=%d higher than CGROUP_SUBSYS_COUNT=%d",
				fields[0],
				idx,
				cgroupSubSysCount,
			)
		}

		if id, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
			cgroupControllers = append(cgroupControllers, cgroupController{
				id:     id,
				idx:    idx,
				name:   fields[0],
				active: true,
			})
		}

		idx++
	}

	return cgroupControllers, nil
}
