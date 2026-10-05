package vmware

import (
	"context"
	"fmt"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"vmware-exporter/pkg/logging"
)

var _ Service = &service{}

type service struct {
	logger         *logging.Logger
	vmwareHost     string
	vmwareUser     string
	vmwarePassword string
	scrapeTimeout  time.Duration
}

type Service interface {
	status() (*Status, error)
	error(err error)
}

var interval = 20

const (
	defaultScrapeTimeout = 60 * time.Second
	// logoutTimeout limits cleanup calls, which must not depend on the (possibly expired) scrape context.
	logoutTimeout = 10 * time.Second
)

// parseTimeout accepts a Go duration ("30s") or a plain number of seconds ("30").
func parseTimeout(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

func NewService(l *logging.Logger, host string, config interface{}) Service {
	cfg := reflect.ValueOf(config).Elem()

	vmwareUser := cfg.FieldByName("VmwareUser").Interface().(string)
	if len(vmwareUser) == 0 {
		l.Debug("user name for VMWare auth not specified, use default user name")
		vmwareUser = "monitoring"
	}

	vmwarePass := cfg.FieldByName("VmwarePass").Interface().(string)
	if len(vmwarePass) == 0 {
		l.Debug("user password for VMWare auth not specified, use default password")
		vmwarePass = "password"
	}

	scrapeTimeout := parseTimeout(cfg.FieldByName("ScrapeTimeout").Interface().(string), defaultScrapeTimeout)
	if scrapeTimeout == defaultScrapeTimeout {
		l.Debug("scrape timeout not specified or invalid, use default timeout")
	}

	return &service{
		logger:         l,
		vmwareHost:     host,
		vmwareUser:     vmwareUser,
		vmwarePassword: vmwarePass,
		scrapeTimeout:  scrapeTimeout,
	}
}

func (s *service) error(err error) {
	s.logger.Error(err.Error())
}

func (s *service) status() (*Status, error) {
	status := Status{
		HostName:            "",
		HostPowerState:      0,
		HostMaintenanceMode: 0,
		HostBoot:            0,
		TotalCpu:            0,
		UsageCpu:            0,
		TotalMem:            0,
		UsageMem:            0,
		DiskOk:              []diskOk{},
		NetworkPNICSpeed:    []pnic{},
		HW:                  hwInfo{},
		Product:             ProductInfo{},
		SensorInfo:          []NumericSensorInfo{},
		StorageInfo:         []StorageStateInfo{},
		DS:                  []totalds{},
		VMS:                 []hvms{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.scrapeTimeout)
	defer cancel()
	c, err := NewClient(ctx, s.vmwareHost, s.vmwareUser, s.vmwarePassword)
	if err != nil {
		return nil, err
	}
	defer func() {
		// the scrape ctx may already be expired, use a fresh one so the session is always released
		lctx, lcancel := context.WithTimeout(context.Background(), logoutTimeout)
		defer lcancel()
		if err := c.Logout(lctx); err != nil {
			s.logger.Error(fmt.Sprintf("logout from %s failed: %v", s.vmwareHost, err))
		}
	}()
	m := view.NewManager(c.Client)

	var hss []mo.HostSystem
	if err := retrieveView(ctx, m, c.ServiceContent.RootFolder, "HostSystem", []string{"summary"}, &hss); err != nil {
		return nil, fmt.Errorf("retrieve HostSystem: %w", err)
	}
	if len(hss) == 0 {
		return nil, fmt.Errorf("host %s returned no HostSystem objects", s.vmwareHost)
	}

	finder := find.NewFinder(c.Client)
	hs, err := finder.DefaultHostSystem(ctx)
	if err != nil {
		return nil, fmt.Errorf("find default host system: %w", err)
	}
	ss, err := hs.ConfigManager().StorageSystem(ctx)
	if err != nil {
		return nil, fmt.Errorf("get storage system: %w", err)
	}
	var hostss mo.HostStorageSystem
	if err := ss.Properties(ctx, ss.Reference(), nil, &hostss); err != nil {
		return nil, fmt.Errorf("get storage system properties: %w", err)
	}

	if len(hostss.StorageDeviceInfo.ScsiLun) > 0 {
		for _, e := range hostss.StorageDeviceInfo.ScsiLun {
			lun := e.GetScsiLun()
			ok := 1.0
			for _, s := range lun.OperationalState {
				if s != "ok" {
					ok = 0
					break
				}
			}
			status.DiskOk = append(status.DiskOk, diskOk{lun.DeviceName, ok})
		}
	}

	nn, err := hs.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		return nil, fmt.Errorf("get network system: %w", err)
	}
	var hostsn mo.HostNetworkSystem
	if err := nn.Properties(ctx, nn.Reference(), nil, &hostsn); err != nil {
		return nil, fmt.Errorf("get network system properties: %w", err)
	}
	for _, ni := range hostsn.NetworkInfo.Pnic {
		var lSpeed float64
		if ni.LinkSpeed != nil {
			lSpeed = float64(ni.LinkSpeed.SpeedMb)
		}

		status.NetworkPNICSpeed = append(status.NetworkPNICSpeed, pnic{ni.Device, ni.Mac, lSpeed})
	}

	// Datastore Metrics
	var dss []mo.Datastore
	if err := retrieveView(ctx, m, c.ServiceContent.RootFolder, "Datastore", []string{"summary"}, &dss); err != nil {
		return nil, fmt.Errorf("retrieve Datastore: %w", err)
	}
	for _, ds := range dss {
		status.DS = append(status.DS, totalds{ds.Summary.Name, float64(ds.Summary.Capacity), float64(ds.Summary.FreeSpace)})
	}

	// Guest VM Metrics
	var vms []mo.VirtualMachine
	if err := retrieveView(ctx, m, c.ServiceContent.RootFolder, "VirtualMachine", nil, &vms); err != nil {
		return nil, fmt.Errorf("retrieve VirtualMachine: %w", err)
	}

	vmsRefs := make([]types.ManagedObjectReference, 0, len(vms))
	for _, vm := range vms {
		vmsRefs = append(vmsRefs, vm.Self)
	}

	// perf counters are optional: a failure must not drop all the other metrics
	vmPerfMetrics, err := perfMon(ctx, c, vmsRefs)
	if err != nil {
		s.logger.Error(fmt.Sprintf("collect VM performance metrics from %s failed: %v", s.vmwareHost, err))
	}

	for _, vm := range vms {
		vmNum := vm.GetManagedEntity().Self.Value
		vmname := vm.Summary.Config.Name

		perfMetrics, ok := vmPerfMetrics[vmNum]

		vmPerfRes := vmPerf{
			CPU_COSTOP_SUMMATION:              0,
			CPU_DEMANDENTITLEMENTRATIO_LATEST: 0,
			CPU_DEMAND_AVERAGE:                0,
			CPU_ENTITLEMENT_LATEST:            0,
			CPU_IDLE_SUMMATION:                0,
			CPU_LATENCY_AVERAGE:               0,
			CPU_MAXLIMITED_SUMMATION:          0,
			CPU_OVERLAP_SUMMATION:             0,
			CPU_READINESS_AVERAGE:             0,
			CPU_READY_SUMMATION:               0,
			CPU_RUN_SUMMATION:                 0,
			CPU_SWAPWAIT_SUMMATION:            0,
			CPU_SYSTEM_SUMMATION:              0,
			CPU_USAGEMHZ_AVERAGE:              0,
			CPU_USAGEMHZ_MAXIMUM:              0,
			CPU_USAGEMHZ_MINIMUM:              0,
			CPU_USAGEMHZ_NONE:                 0,
			CPU_USAGE_AVERAGE:                 0,
			CPU_USAGE_MAXIMUM:                 0,
			CPU_USAGE_MINIMUM:                 0,
			CPU_USAGE_NONE:                    0,
			CPU_USED_SUMMATION:                0,
			CPU_WAIT_SUMMATION:                0,
			DATASTORE_MAXTOTALLATENCY_LATEST:  0,
			DISK_MAXTOTALLATENCY_LATEST:       0,
			DISK_READ_AVERAGE:                 0,
			DISK_USAGE_AVERAGE:                0,
			DISK_USAGE_MAXIMUM:                0,
			DISK_USAGE_MINIMUM:                0,
			DISK_USAGE_NONE:                   0,
			DISK_WRITE_AVERAGE:                0,
			MEM_ACTIVEWRITE_AVERAGE:           0,
			MEM_ACTIVE_AVERAGE:                0,
			MEM_ACTIVE_MAXIMUM:                0,
			MEM_ACTIVE_MINIMUM:                0,
			MEM_ACTIVE_NONE:                   0,
			MEM_COMPRESSED_AVERAGE:            0,
			MEM_COMPRESSIONRATE_AVERAGE:       0,
			MEM_CONSUMED_AVERAGE:              0,
			MEM_CONSUMED_MAXIMUM:              0,
			MEM_CONSUMED_MINIMUM:              0,
			MEM_CONSUMED_NONE:                 0,
			MEM_DECOMPRESSIONRATE_AVERAGE:     0,
			MEM_ENTITLEMENT_AVERAGE:           0,
			MEM_GRANTED_AVERAGE:               0,
			MEM_GRANTED_MAXIMUM:               0,
			MEM_GRANTED_MINIMUM:               0,
			MEM_GRANTED_NONE:                  0,
			MEM_LATENCY_AVERAGE:               0,
			MEM_LLSWAPINRATE_AVERAGE:          0,
			MEM_LLSWAPOUTRATE_AVERAGE:         0,
			MEM_LLSWAPUSED_AVERAGE:            0,
			MEM_LLSWAPUSED_MAXIMUM:            0,
			MEM_LLSWAPUSED_MINIMUM:            0,
			MEM_LLSWAPUSED_NONE:               0,
			MEM_OVERHEADMAX_AVERAGE:           0,
			MEM_OVERHEADTOUCHED_AVERAGE:       0,
			MEM_OVERHEAD_AVERAGE:              0,
			MEM_OVERHEAD_MAXIMUM:              0,
			MEM_OVERHEAD_MINIMUM:              0,
			MEM_OVERHEAD_NONE:                 0,
			MEM_SHARED_AVERAGE:                0,
			MEM_SHARED_MAXIMUM:                0,
			MEM_SHARED_MINIMUM:                0,
			MEM_SHARED_NONE:                   0,
			MEM_SWAPINRATE_AVERAGE:            0,
			MEM_SWAPIN_AVERAGE:                0,
			MEM_SWAPIN_MAXIMUM:                0,
			MEM_SWAPIN_MINIMUM:                0,
			MEM_SWAPIN_NONE:                   0,
			MEM_SWAPOUTRATE_AVERAGE:           0,
			MEM_SWAPOUT_AVERAGE:               0,
			MEM_SWAPOUT_MAXIMUM:               0,
			MEM_SWAPOUT_MINIMUM:               0,
			MEM_SWAPOUT_NONE:                  0,
			MEM_SWAPPED_AVERAGE:               0,
			MEM_SWAPPED_MAXIMUM:               0,
			MEM_SWAPPED_MINIMUM:               0,
			MEM_SWAPPED_NONE:                  0,
			MEM_SWAPTARGET_AVERAGE:            0,
			MEM_SWAPTARGET_MAXIMUM:            0,
			MEM_SWAPTARGET_MINIMUM:            0,
			MEM_SWAPTARGET_NONE:               0,
			MEM_USAGE_AVERAGE:                 0,
			MEM_USAGE_MAXIMUM:                 0,
			MEM_USAGE_MINIMUM:                 0,
			MEM_USAGE_NONE:                    0,
			MEM_VMMEMCTLTARGET_AVERAGE:        0,
			MEM_VMMEMCTLTARGET_MAXIMUM:        0,
			MEM_VMMEMCTLTARGET_MINIMUM:        0,
			MEM_VMMEMCTLTARGET_NONE:           0,
			MEM_VMMEMCTL_AVERAGE:              0,
			MEM_VMMEMCTL_MAXIMUM:              0,
			MEM_VMMEMCTL_MINIMUM:              0,
			MEM_VMMEMCTL_NONE:                 0,
			MEM_ZERO_AVERAGE:                  0,
			MEM_ZERO_MAXIMUM:                  0,
			MEM_ZERO_MINIMUM:                  0,
			MEM_ZERO_NONE:                     0,
			MEM_ZIPPED_LATEST:                 0,
			MEM_ZIPSAVED_LATEST:               0,
			NET_BROADCASTRX_SUMMATION:         0,
			NET_BROADCASTTX_SUMMATION:         0,
			NET_BYTESRX_AVERAGE:               0,
			NET_BYTESTX_AVERAGE:               0,
			NET_DROPPEDRX_SUMMATION:           0,
			NET_DROPPEDTX_SUMMATION:           0,
			NET_MULTICASTRX_SUMMATION:         0,
			NET_MULTICASTTX_SUMMATION:         0,
			NET_PACKETSRX_SUMMATION:           0,
			NET_PACKETSTX_SUMMATION:           0,
			NET_PNICBYTESRX_AVERAGE:           0,
			NET_PNICBYTESTX_AVERAGE:           0,
			NET_RECEIVED_AVERAGE:              0,
			NET_TRANSMITTED_AVERAGE:           0,
			NET_USAGE_AVERAGE:                 0,
			NET_USAGE_MAXIMUM:                 0,
			NET_USAGE_MINIMUM:                 0,
			NET_USAGE_NONE:                    0,
			POWER_ENERGY_SUMMATION:            0,
			POWER_POWER_AVERAGE:               0,
			RESCPU_ACTAV15_LATEST:             0,
			RESCPU_ACTAV1_LATEST:              0,
			RESCPU_ACTAV5_LATEST:              0,
			RESCPU_ACTPK15_LATEST:             0,
			RESCPU_ACTPK1_LATEST:              0,
			RESCPU_ACTPK5_LATEST:              0,
			RESCPU_MAXLIMITED15_LATEST:        0,
			RESCPU_MAXLIMITED1_LATEST:         0,
			RESCPU_MAXLIMITED5_LATEST:         0,
			RESCPU_RUNAV15_LATEST:             0,
			RESCPU_RUNAV1_LATEST:              0,
			RESCPU_RUNAV5_LATEST:              0,
			RESCPU_RUNPK15_LATEST:             0,
			RESCPU_RUNPK1_LATEST:              0,
			RESCPU_RUNPK5_LATEST:              0,
			RESCPU_SAMPLECOUNT_LATEST:         0,
			RESCPU_SAMPLEPERIOD_LATEST:        0,
			SYS_HEARTBEAT_LATEST:              0,
			SYS_OSUPTIME_LATEST:               0,
			SYS_UPTIME_LATEST:                 0,
			VIRTUALDISK_READ_AVERAGE:          0,
			VIRTUALDISK_WRITE_AVERAGE:         0,
		}

		if ok {
			for _, v := range perfMetrics {
				value, err := strconv.Atoi(v.MetricValue)
				if err == nil {
					metricName := strings.ToUpper(strings.Replace(v.MetricName, ".", "_", -1))
					metricValue := float64(value)

					r := reflect.ValueOf(&vmPerfRes).Elem()
					if r.Kind() == reflect.Struct {
						f := r.FieldByName(metricName)
						if f.IsValid() {
							if f.CanSet() {
								if f.Kind() == reflect.Float64 {
									f.SetFloat(metricValue)
								}
							}
						}
					}
					//r.FieldByName(metricName).SetFloat(metricValue)
				}

			}
		}

		status.VMS = append(status.VMS, hvms{
			VmName:                    vmname,
			VmPowerState:              powerStateVM(vm.Summary.Runtime.PowerState),
			VmGuestId:                 vm.Summary.Guest.GuestId,
			VmGuestToolsStatus:        guestToolsStatus(string(vm.Summary.Guest.ToolsStatus)),
			VmGuestToolsVersion:       guestToolsVersion(vm.Summary.Guest.ToolsVersionStatus),
			VmGuestFullName:           vm.Summary.Guest.GuestFullName,
			VmGuestIpAddr:             vm.Summary.Guest.IpAddress,
			VmGuestStorageCommitted:   float64(vm.Summary.Storage.Committed),
			VmGuestStorageUnCommitted: float64(vm.Summary.Storage.Uncommitted),
			VmBoot:                    convertTime(vm),
			VmCpuAval:                 float64(vm.Summary.Runtime.MaxCpuUsage),
			VmCpuUsage:                float64(vm.Summary.QuickStats.OverallCpuUsage),
			VmNumCpu:                  float64(vm.Summary.Config.NumCpu),
			VmMemAval:                 float64(vm.Summary.Config.MemorySizeMB),
			VmMemUsage:                float64(vm.Summary.QuickStats.HostMemoryUsage),
			Perf:                      vmPerfRes,
		})

	}

	if len(hss[0].Summary.Runtime.HealthSystemRuntime.SystemHealthInfo.NumericSensorInfo) > 0 {
		for _, sensor := range hss[0].Summary.Runtime.HealthSystemRuntime.SystemHealthInfo.NumericSensorInfo {
			status.SensorInfo = append(status.SensorInfo, NumericSensorInfo{
				Name:           sensor.Name,
				HealthState:    sensorHealthOf(sensor.HealthState),
				CurrentReading: strconv.Itoa(int(float64(sensor.CurrentReading) * math.Pow(10, float64(sensor.UnitModifier)))),
				BaseUnits:      sensor.BaseUnits,
				SensorType:     sensor.SensorType,
				Id:             sensor.Id,
				SensorNumber:   strconv.Itoa(int(sensor.SensorNumber)),
			})
		}
	}

	if len(hss[0].Summary.Runtime.HealthSystemRuntime.HardwareStatusInfo.StorageStatusInfo) > 0 {
		for _, storageSensor := range hss[0].Summary.Runtime.HealthSystemRuntime.HardwareStatusInfo.StorageStatusInfo {
			if !checkSensorIfAppend(storageSensor.Name, status.StorageInfo) {
				status.StorageInfo = append(status.StorageInfo, StorageStateInfo{
					Name:   storageSensor.Name,
					Status: sensorHealthOf(storageSensor.Status),
				})
			}
		}
	}

	status.HostName = hss[0].Summary.Config.Name
	status.HostPowerState = powerState(hss[0].Summary.Runtime.PowerState)
	status.HostMaintenanceMode = maintenanceMode(hss[0].Summary.Runtime.InMaintenanceMode)
	if bt := hss[0].Summary.Runtime.BootTime; bt != nil {
		status.HostBoot = float64(bt.Unix())
	}
	status.TotalCpu = totalCpu(hss[0])
	status.UsageCpu = float64(hss[0].Summary.QuickStats.OverallCpuUsage)
	status.TotalMem = float64(hss[0].Summary.Hardware.MemorySize)
	status.UsageMem = float64(hss[0].Summary.QuickStats.OverallMemoryUsage) * 1024 * 1024
	status.HW.Vendor = hss[0].Summary.Hardware.Vendor
	status.HW.Model = hss[0].Summary.Hardware.Model
	status.HW.Uuid = hss[0].Summary.Hardware.Uuid
	status.HW.CpuModel = hss[0].Summary.Hardware.CpuModel
	status.HW.NumCpuPkgs = float64(hss[0].Summary.Hardware.NumCpuPkgs)
	status.HW.CpuMhz = float64(hss[0].Summary.Hardware.CpuMhz)
	status.HW.NumCpuCores = float64(hss[0].Summary.Hardware.NumCpuCores)
	status.HW.NumCpuThreads = float64(hss[0].Summary.Hardware.NumCpuThreads)
	status.HW.NumNics = float64(hss[0].Summary.Hardware.NumNics)
	status.HW.NumHBAs = float64(hss[0].Summary.Hardware.NumHBAs)
	status.Product.Name = hss[0].Summary.Config.Product.Name
	status.Product.FullName = hss[0].Summary.Config.Product.FullName
	status.Product.Vendor = hss[0].Summary.Config.Product.Vendor
	status.Product.Version = hss[0].Summary.Config.Product.Version
	status.Product.Build = hss[0].Summary.Config.Product.Build
	status.Product.OsType = hss[0].Summary.Config.Product.OsType
	status.Product.ApiVersion = hss[0].Summary.Config.Product.ApiVersion
	status.Product.LicenseProductName = hss[0].Summary.Config.Product.LicenseProductName
	status.Product.LicenseVersion = hss[0].Summary.Config.Product.LicenseProductVersion

	return &status, nil
}

// retrieveView loads properties of all objects of the given type and always destroys the view,
// even when the scrape context is already expired.
func retrieveView(ctx context.Context, m *view.Manager, root types.ManagedObjectReference, kind string, props []string, dst interface{}) error {
	v, err := m.CreateContainerView(ctx, root, []string{kind}, true)
	if err != nil {
		return err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
		defer cancel()
		_ = v.Destroy(dctx)
	}()
	return v.Retrieve(ctx, []string{kind}, props, dst)
}

func checkSensorIfAppend(name string, sensors []StorageStateInfo) bool {
	for _, sensor := range sensors {
		if sensor.Name == name {
			return true
		}
	}
	return false
}
