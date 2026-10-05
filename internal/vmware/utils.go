package vmware

import (
	"context"
	"fmt"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"strings"
)

type vmMetric struct {
	Instance    string
	MetricName  string
	MetricValue string
	MetricUnit  string
}

func totalCpu(hs mo.HostSystem) float64 {
	totalCPU := int64(hs.Summary.Hardware.CpuMhz) * int64(hs.Summary.Hardware.NumCpuCores)
	return float64(totalCPU)
}

func convertTime(vm mo.VirtualMachine) float64 {
	if vm.Summary.Runtime.BootTime == nil {
		return 0
	}
	return float64(vm.Summary.Runtime.BootTime.Unix())
}

func powerState(s types.HostSystemPowerState) float64 {
	if s == "poweredOn" {
		return 1
	}
	if s == "poweredOff" {
		return 2
	}
	if s == "standBy" {
		return 3
	}
	return 0
}

func sensorHealth(s string) float64 {
	if s == "green" {
		return 1
	}
	if s == "yellow" {
		return 2
	}
	if s == "red" {
		return 3
	}
	return 0
}

// sensorHealthOf is nil-safe: hosts may omit the health state of a sensor.
func sensorHealthOf(s types.BaseElementDescription) float64 {
	if s == nil {
		return 0
	}
	return sensorHealth(strings.ToLower(s.GetElementDescription().Key))
}

func powerStateVM(s types.VirtualMachinePowerState) float64 {
	if s == "poweredOn" {
		return 1
	}
	if s == "poweredOff" {
		return 2
	}
	if s == "standBy" {
		return 3
	}
	return 0
}

func guestToolsStatus(s string) float64 {
	if s == "toolsOk" {
		return 1
	}

	return 0
}

func guestToolsVersion(s string) float64 {
	if s == "guestToolsUnmanaged" {
		return 0
	}
	return 1
}

func maintenanceMode(s bool) float64 {
	if s {
		return 1
	}
	return 0
}

// perfMon returns performance counters per VM (key is the VM managed object id).
func perfMon(ctx context.Context, c *govmomi.Client, vms []types.ManagedObjectReference) (map[string][]vmMetric, error) {
	if len(vms) == 0 {
		return nil, nil
	}

	// Create a PerfManager
	perfManager := performance.NewManager(c.Client)

	// Retrieve counters name list
	counters, err := perfManager.CounterInfoByName(ctx)
	if err != nil {
		return nil, fmt.Errorf("counter info: %w", err)
	}

	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}

	// Create PerfQuerySpec
	spec := types.PerfQuerySpec{
		MaxSample:  1,
		MetricId:   []types.PerfMetricId{{Instance: ""}},
		IntervalId: int32(interval),
	}

	// Query metrics
	sample, err := perfManager.SampleByName(ctx, spec, names, vms)
	if err != nil {
		return nil, fmt.Errorf("sample: %w", err)
	}

	result, err := perfManager.ToMetricSeries(ctx, sample)
	if err != nil {
		return nil, fmt.Errorf("metric series: %w", err)
	}

	metricsRes := make(map[string][]vmMetric)
	for _, metric := range result {
		var vmMetrics []vmMetric
		for _, v := range metric.Value {
			if len(v.Value) == 0 {
				continue
			}
			instance := v.Instance
			if instance == "" {
				instance = "-"
			}
			var unit string
			if ci, ok := counters[v.Name]; ok && ci.UnitInfo != nil {
				unit = ci.UnitInfo.GetElementDescription().Label
			}
			vmMetrics = append(vmMetrics, vmMetric{
				Instance:    instance,
				MetricName:  v.Name,
				MetricValue: v.ValueCSV(),
				MetricUnit:  unit,
			})
		}
		metricsRes[metric.Entity.Value] = vmMetrics
	}
	return metricsRes, nil
}
