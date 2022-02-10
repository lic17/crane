//go:build linux
// +build linux

package nodelocal

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	cmemory "github.com/google/cadvisor/cache/memory"
	cadvisorcontainer "github.com/google/cadvisor/container"
	info "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	cmanager "github.com/google/cadvisor/manager"
	csysfs "github.com/google/cadvisor/utils/sysfs"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/gocrane/crane/pkg/common"
	"github.com/gocrane/crane/pkg/ensurance/collector/types"
	"github.com/gocrane/crane/pkg/utils"
)

const (
	cadvisorCollectorName = "cadvisor"
)

func init() {
	registerMetrics(cadvisorCollectorName, []types.MetricName{types.MetricNameContainerCpuTotalUsage, types.MetricNameContainerSchedRunQueueTime}, NewCadvisor)
}

type CgroupState struct {
	stat      cadvisorapiv2.ContainerInfo
	timestamp time.Time
}

//CadvisorCollector is the collector to collect container state
type CadvisorCollector struct {
	Manager   cmanager.Manager
	podLister corelisters.PodLister

	cgroupState           map[string]CgroupState
	MemCache              *cmemory.InMemoryCache
	SysFs                 csysfs.SysFs
	IncludeMetrics        cadvisorcontainer.MetricSet
	MaxHousekeepingConfig cmanager.HouskeepingConfig
}

func NewCadvisor(context *NodeLocalContext) (nodeLocalCollector, error) {

	var includedMetrics = cadvisorcontainer.MetricSet{
		cadvisorcontainer.CpuUsageMetrics:         struct{}{},
		cadvisorcontainer.ProcessSchedulerMetrics: struct{}{},
	}

	var allowDynamic bool = true
	var maxHousekeepingInterval time.Duration = 10 * time.Second
	var memCache = cmemory.New(10*time.Minute, nil)
	var sysfs = csysfs.NewRealSysFs()
	var maxHousekeepingConfig = cmanager.HouskeepingConfig{Interval: &maxHousekeepingInterval, AllowDynamic: &allowDynamic}

	m, err := cmanager.New(memCache, sysfs, maxHousekeepingConfig, includedMetrics, http.DefaultClient, []string{utils.CgroupKubePods}, "")
	if err != nil {
		return nil, fmt.Errorf("cadvisor manager start err: %s", err.Error())
	}

	c := CadvisorCollector{
		Manager:   m,
		podLister: context.PodLister,
	}

	if err := c.Manager.Start(); err != nil {
		return nil, err
	}

	return &c, nil
}

// Start cadvisor manager
func (c *CadvisorCollector) Start() error {
	return c.Manager.Start()
}

// Stop cadvisor and clear existing factory
func (c *CadvisorCollector) Stop() error {
	if err := c.Manager.Stop(); err != nil {
		return err
	}

	// clear existing factory
	cadvisorcontainer.ClearContainerHandlerFactories()

	return nil
}

func (c *CadvisorCollector) name() string {
	return cadvisorCollectorName
}

func (c *CadvisorCollector) collect() (map[string][]common.TimeSeries, error) {
	var cgroupState = make(map[string]CgroupState)

	allPods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list all pods: %v", err)
		return nil, err
	}

	var stateMap = make(map[string][]common.TimeSeries)
	for _, pod := range allPods {
		var now = time.Now()
		containers, err := c.Manager.GetContainerInfoV2(types.GetCgroupPath(pod), cadvisorapiv2.RequestOptions{
			IdType:    cadvisorapiv2.TypeName,
			Count:     1,
			Recursive: true,
		})
		if err != nil {
			klog.Errorf("GetContainerInfoV2 failed: %v", err)
			continue
		}

		for key, v := range containers {
			containerId := utils.GetContainerIdFromKey(key)
			containerName := GetContainerNameFromPod(pod, containerId)
			// Filter the sandbox container
			if (containerId != "") && (containerName == "") {
				continue
			}
			// In the GetContainerInfoV2 not collect the cpu quota and period
			// We used GetContainerInfo instead
			// issue https://github.com/google/cadvisor/issues/3040
			var query = info.ContainerInfoRequest{}
			containerInfoV1, err := c.Manager.GetContainerInfo(key, &query)
			if err != nil {
				klog.Errorf("ContainerInfoRequest failed: %v", err)
				continue
			}

			if state, ok := c.cgroupState[key]; ok {
				var labels = GetContainerLabels(pod, containerId, containerName)

				cpuUsageSample, schedRunqueueTime := CaculateCPUUsage(&v, &state)
				if cpuUsageSample == 0 && schedRunqueueTime == 0 {
					continue
				}
				addSampleToStateMap(types.MetricNameContainerCpuTotalUsage, composeSample(labels, cpuUsageSample, now), stateMap)
				addSampleToStateMap(types.MetricNameContainerSchedRunQueueTime, composeSample(labels, schedRunqueueTime, now), stateMap)
				addSampleToStateMap(types.MetricNameContainerCpuLimit, composeSample(labels, float64(state.stat.Spec.Cpu.Limit), now), stateMap)
				addSampleToStateMap(types.MetricNameContainerCpuQuota, composeSample(labels, float64(containerInfoV1.Spec.Cpu.Quota), now), stateMap)
				addSampleToStateMap(types.MetricNameContainerCpuPeriod, composeSample(labels, float64(containerInfoV1.Spec.Cpu.Period), now), stateMap)

				klog.V(4).Infof("Pod: %s, containerName: %s, key %s, scheduler run queue time %.2f", klog.KObj(pod), containerName, key, schedRunqueueTime)
			}
			cgroupState[key] = CgroupState{stat: v, timestamp: now}
		}
	}

	c.cgroupState = cgroupState

	return stateMap, nil
}

func composeSample(labels []common.Label, cpuUsageSample float64, sampleTime time.Time) common.TimeSeries {
	return common.TimeSeries{
		Labels: labels,
		Samples: []common.Sample{
			{
				Value:     cpuUsageSample,
				Timestamp: sampleTime.Unix(),
			},
		},
	}
}

func addSampleToStateMap(metricsName types.MetricName, usage common.TimeSeries, storeMaps map[string][]common.TimeSeries) {
	key := string(metricsName)
	if _, exists := storeMaps[key]; !exists {
		storeMaps[key] = []common.TimeSeries{usage}
	} else {
		storeMaps[key] = append(storeMaps[key], usage)
	}
}

func CaculateCPUUsage(info *cadvisorapiv2.ContainerInfo, state *CgroupState) (float64, float64) {
	if info == nil ||
		state == nil ||
		len(info.Stats) == 0 {
		return 0, 0
	}
	cpuUsageIncrease := info.Stats[0].Cpu.Usage.Total - state.stat.Stats[0].Cpu.Usage.Total
	schedRunqueueTimeIncrease := info.Stats[0].Cpu.Schedstat.RunqueueTime - state.stat.Stats[0].Cpu.Schedstat.RunqueueTime
	timeIncrease := info.Stats[0].Timestamp.UnixNano() - state.stat.Stats[0].Timestamp.UnixNano()
	cpuUsageSample := float64(cpuUsageIncrease) / float64(timeIncrease)
	schedRunqueueTime := float64(schedRunqueueTimeIncrease) * 1000 * 1000 / float64(timeIncrease)
	return cpuUsageSample, schedRunqueueTime
}

func GetContainerNameFromPod(pod *v1.Pod, containerId string) string {
	if containerId == "" {
		return ""
	}

	for _, v := range pod.Status.ContainerStatuses {
		strList := strings.Split(v.ContainerID, "//")
		if len(strList) > 0 {
			if strList[len(strList)-1] == containerId {
				return v.Name
			}
		}
	}

	return ""
}

func GetContainerLabels(pod *v1.Pod, containerId, containerName string) []common.Label {
	return []common.Label{
		{Name: common.LabelNamePodName, Value: pod.Name},
		{Name: common.LabelNamePodNamespace, Value: pod.Namespace},
		{Name: common.LabelNamePodUid, Value: string(pod.UID)},
		{Name: common.LabelNameContainerName, Value: containerName},
		{Name: common.LabelNameContainerId, Value: containerId},
	}
}