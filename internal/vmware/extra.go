package vmware

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Performance counters read from the hosts and from the VMs (instanced ones come per NIC, datastore, ...).
// Only counters that return data on ESXi 8 and vCenter 8 are listed.
var (
	hostCounters = []string{
		"cpu.usage.average", "cpu.usagemhz.average", "cpu.demand.average", "cpu.latency.average",
		"cpu.ready.summation", "cpu.readiness.average", "cpu.costop.summation",
		"mem.usage.average", "mem.active.average", "mem.consumed.average", "mem.granted.average",
		"mem.shared.average", "mem.sysUsage.average", "mem.swapused.average", "mem.vmmemctl.average",
		"disk.maxTotalLatency.latest", "disk.usage.average", "power.power.average", "sys.uptime.latest",
	}
	hostInstancedCounters = []string{
		"net.bytesRx.average", "net.bytesTx.average", "net.packetsRx.summation", "net.packetsTx.summation",
		"net.errorsRx.summation", "net.errorsTx.summation", "net.droppedRx.summation", "net.droppedTx.summation",
		"datastore.read.average", "datastore.write.average",
		"datastore.numberReadAveraged.average", "datastore.numberWriteAveraged.average",
		"datastore.totalReadLatency.average", "datastore.totalWriteLatency.average",
		"disk.read.average", "disk.write.average",
	}
	vmInstancedCounters = []string{
		"datastore.read.average", "datastore.write.average",
		"datastore.numberReadAveraged.average", "datastore.numberWriteAveraged.average",
		"datastore.totalReadLatency.average", "datastore.totalWriteLatency.average",
	}
)

// extraDescs describes metrics that do not fit the fixed Collector fields.
type extraDescs struct {
	vcenterInfo       *prometheus.Desc
	hostConnected     *prometheus.Desc
	hostClusterInfo   *prometheus.Desc
	dsInfo            *prometheus.Desc
	dsAccessible      *prometheus.Desc
	dsUncommitted     *prometheus.Desc
	dsProvisioned     *prometheus.Desc
	vmSnapshotCount   *prometheus.Desc
	vmSnapshotCreated *prometheus.Desc
	vmDSCommitted     *prometheus.Desc
	vmDSUncommitted   *prometheus.Desc
	vmDSUnshared      *prometheus.Desc

	hostPerf     map[string]*prometheus.Desc // counter -> desc, labels: host_name
	hostInstPerf map[string]*prometheus.Desc // labels: host_name, instance
	vmDSPerf     map[string]*prometheus.Desc // labels: vm_name, host_name, instance
}

// perfMetricName turns a counter like "net.bytesRx.average" into "<prefix>_net_bytesrx_average".
func perfMetricName(prefix, counter string) string {
	return prefix + "_" + strings.ToLower(strings.ReplaceAll(counter, ".", "_"))
}

func newPerfDescs(prefix string, counters []string, labels []string) map[string]*prometheus.Desc {
	m := make(map[string]*prometheus.Desc, len(counters))
	for _, c := range counters {
		m[c] = prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", perfMetricName(prefix, c)),
			fmt.Sprintf("Vmware performance counter %s (real-time sample, vSphere units)", c),
			labels, nil,
		)
	}
	return m
}

func newExtraDescs() *extraDescs {
	desc := func(sub, name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, sub, name), help, labels, nil)
	}
	return &extraDescs{
		vcenterInfo:       desc("vcenter", "info", "Vmware vCenter version info", "version", "build", "patch", "full_name", "api_type"),
		hostConnected:     desc("host", "connected", "Vmware Host connection state to vCenter, connected 1, other 0", "host_name"),
		hostClusterInfo:   desc("host", "cluster_info", "Vmware Host membership in a cluster", "host_name", "cluster"),
		dsInfo:            desc("datastore", "info", "Datastore type and url", "ds_name", "host_name", "type", "url"),
		dsAccessible:      desc("datastore", "accessible", "Datastore is accessible 1, not 0", "ds_name", "host_name"),
		dsUncommitted:     desc("datastore", "uncommitted_size", "Datastore uncommitted (thin provisioned, not yet allocated) space in bytes", "ds_name", "host_name"),
		dsProvisioned:     desc("datastore", "provisioned_size", "Datastore provisioned space in bytes: used + uncommitted", "ds_name", "host_name"),
		vmSnapshotCount:   desc("vm", "snapshot_count", "Number of VM snapshots", "vm_name", "host_name"),
		vmSnapshotCreated: desc("vm", "snapshot_created_timestamp_seconds", "VM snapshot creation time, unix timestamp", "vm_name", "host_name", "snapshot_name", "snapshot_id"),
		vmDSCommitted:     desc("vm", "datastore_committed_size", "VM space committed on a datastore in bytes", "vm_name", "host_name", "ds_name"),
		vmDSUncommitted:   desc("vm", "datastore_uncommitted_size", "VM space uncommitted on a datastore in bytes", "vm_name", "host_name", "ds_name"),
		vmDSUnshared:      desc("vm", "datastore_unshared_size", "VM unshared space on a datastore in bytes", "vm_name", "host_name", "ds_name"),

		hostPerf:     newPerfDescs("host", hostCounters, []string{"host_name"}),
		hostInstPerf: newPerfDescs("host", hostInstancedCounters, []string{"host_name", "instance"}),
		vmDSPerf:     newPerfDescs("vm", vmInstancedCounters, []string{"vm_name", "host_name", "instance"}),
	}
}

func (e *extraDescs) describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		e.vcenterInfo, e.hostConnected, e.hostClusterInfo, e.dsInfo, e.dsAccessible, e.dsUncommitted, e.dsProvisioned,
		e.vmSnapshotCount, e.vmSnapshotCreated, e.vmDSCommitted, e.vmDSUncommitted, e.vmDSUnshared,
	} {
		ch <- d
	}
	for _, m := range []map[string]*prometheus.Desc{e.hostPerf, e.hostInstPerf, e.vmDSPerf} {
		for _, d := range m {
			ch <- d
		}
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// collect emits the metrics of one host: the host itself, its datastores and its VMs.
func (e *extraDescs) collect(ch chan<- prometheus.Metric, s *Status) {
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}

	if s.VCenter != nil {
		gauge(e.vcenterInfo, 1, s.VCenter.Version, s.VCenter.Build, s.VCenter.Patch, s.VCenter.FullName, s.VCenter.ApiType)
	}

	gauge(e.hostConnected, s.HostConnected, s.HostName)
	if s.HostCluster != "" {
		gauge(e.hostClusterInfo, 1, s.HostName, s.HostCluster)
	}
	for _, p := range s.HostPerf {
		if p.Instance == "" {
			if d, ok := e.hostPerf[p.Counter]; ok {
				gauge(d, p.Value, s.HostName)
			}
		} else if d, ok := e.hostInstPerf[p.Counter]; ok {
			gauge(d, p.Value, s.HostName, p.Instance)
		}
	}

	for _, ds := range s.DS {
		gauge(e.dsInfo, 1, ds.dsname, s.HostName, ds.dsType, ds.url)
		gauge(e.dsAccessible, boolValue(ds.accessible), ds.dsname, s.HostName)
		gauge(e.dsUncommitted, ds.uncommitted, ds.dsname, s.HostName)
		gauge(e.dsProvisioned, ds.capacity-ds.freespace+ds.uncommitted, ds.dsname, s.HostName)
	}

	for _, vm := range s.VMS {
		gauge(e.vmSnapshotCount, vm.SnapshotCount, vm.VmName, s.HostName)
		// snapshot names are not unique: the id keeps the series distinct
		for _, sn := range vm.Snapshots {
			gauge(e.vmSnapshotCreated, sn.Created, vm.VmName, s.HostName, sn.Name, fmt.Sprint(sn.Id))
		}
		for _, u := range vm.DSUsage {
			gauge(e.vmDSCommitted, u.Committed, vm.VmName, s.HostName, u.DsName)
			gauge(e.vmDSUncommitted, u.Uncommitted, vm.VmName, s.HostName, u.DsName)
			gauge(e.vmDSUnshared, u.Unshared, vm.VmName, s.HostName, u.DsName)
		}
		for _, p := range vm.DSPerf {
			if d, ok := e.vmDSPerf[p.Counter]; ok {
				gauge(d, p.Value, vm.VmName, s.HostName, p.Instance)
			}
		}
	}
}
