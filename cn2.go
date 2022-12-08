package cn2vk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/virtual-kubelet/node-cli/manager"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type Cni struct {
	CniVersion string `json:"cniVersion"`
	Interfaces []struct {
		Name    string `json:"name"`
		Mac     string `json:"mac"`
		Sandbox string `json:"sandbox"`
	} `json:"interfaces"`
	Ips []struct {
		Version   string `json:"version"`
		Interface int    `json:"interface"`
		Address   string `json:"address"`
		Gateway   string `json:"gateway"`
	} `json:"ips"`
	DNS struct {
	} `json:"dns"`
}

type Provider struct {
	resourceManager    *manager.ResourceManager
	nodeName           string
	operatingSystem    string
	internalIP         string
	daemonEndpointPort int32
	podStatus          map[types.UID]string
	notifyStatus       func(*v1.Pod)
	podMap             map[types.NamespacedName]*v1.Pod
	cniPath            string
	logger             log.Logger
}

func NewProvider(nodeName, operatingSystem string, internalIP string, resourceManager *manager.ResourceManager, daemonEndpointPort int32, cniPath string, logger log.Logger) (*Provider, error) {

	provider := Provider{
		resourceManager:    resourceManager,
		nodeName:           nodeName,
		operatingSystem:    operatingSystem,
		internalIP:         internalIP,
		daemonEndpointPort: daemonEndpointPort,
		podStatus:          make(map[types.UID]string),
		podMap:             make(map[types.NamespacedName]*v1.Pod),
		cniPath:            cniPath,
		logger:             logger,
	}

	p := &provider
	return p, nil
}

func (p *Provider) capacity(ctx context.Context) v1.ResourceList {
	var cpuQ resource.Quantity
	cpuQ.Set(int64(8))
	var memQ resource.Quantity
	memQ.Set(int64(128))

	return v1.ResourceList{
		"cpu":    cpuQ,
		"memory": memQ,
		"pods":   resource.MustParse("1000"),
	}
}

func (p *Provider) ConfigureNode(ctx context.Context, node *v1.Node) {
	node.Status.Capacity = p.capacity(ctx)
	node.Status.Conditions = p.nodeConditions()
	node.Status.Addresses = p.nodeAddresses()
	node.Status.DaemonEndpoints = p.nodeDaemonEndpoints()
	node.Status.NodeInfo.OperatingSystem = p.operatingSystem
}
func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {

	pod.Status.HostIP = p.internalIP
	pod.Status.Phase = v1.PodPending
	pod.Generation = 1
	pod.CreationTimestamp = metav1.Now()
	p.podMap[types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}] = pod
	return nil
}

var cniConf = `{
	"cniVersion": "0.4.0",
	"name": "new",
	"type": "loopback"
}`

func (p *Provider) runCNI(podName, podNamespace string) (string, error) {

	var i, o, e bytes.Buffer
	i.Write([]byte(cniConf))

	cmd := exec.Command(p.cniPath)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "CNI_COMMAND=ADD")
	cmd.Env = append(cmd.Env, "CNI_CONTAINERID=1")
	cmd.Env = append(cmd.Env, "CNI_NETNS=1")
	cmd.Env = append(cmd.Env, "CNI_IFNAME=eth1")
	cmd.Env = append(cmd.Env, "CNI_PATH=/bin")
	cmd.Env = append(cmd.Env, fmt.Sprintf("CNI_ARGS=K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s", podName, podNamespace))
	cmd.Stdout = &o
	cmd.Stdin = &i
	cmd.Stderr = &e
	if err := cmd.Run(); err != nil {
		return e.String(), err
	}

	return o.String(), nil
	//CNI_COMMAND=ADD CNI_CONTAINERID=1 CNI_NETNS=1 CNI_IFNAME=eth1 CNI_PATH=/bin CNI_ARGS="K8S_POD_NAME=pod;K8S_POD_NAMESPACE=default" go run main.go < cni.conf
}

func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	fmt.Println("POD UPDATE")
	res, err := p.runCNI(pod.Name, pod.Namespace)
	if err != nil {
		p.logger.Error(err)
		return err
	}

	cni := &Cni{}
	if err := json.Unmarshal([]byte(res), cni); err != nil {
		p.logger.Error(err)
		return err
	}

	var v4ip string
	var podIps []v1.PodIP

	for _, ip := range cni.Ips {
		if ip.Version == "4" {
			v4ip = strings.Split(ip.Address, "/")[0]
		}
		podIps = append(podIps, v1.PodIP{
			IP: strings.Split(ip.Address, "/")[0],
		})
	}

	var containerStatusList []v1.ContainerStatus
	for _, container := range pod.Spec.Containers {
		containerStatus := v1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			Ready: true,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{
					StartedAt: metav1.Now(),
				},
			},
		}
		containerStatusList = append(containerStatusList, containerStatus)
	}

	pod.Status.PodIP = v4ip
	pod.Status.PodIPs = podIps
	pod.Generation = pod.Generation + 1
	pod.Status.Phase = v1.PodRunning
	startTime := metav1.Now()
	pod.Status.StartTime = &startTime
	pod.Status.ContainerStatuses = containerStatusList
	p.podMap[types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}] = pod
	return nil
}
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	delete(p.podMap, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
	return nil
}
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {

	return p.podMap[types.NamespacedName{Name: name, Namespace: namespace}], nil
}

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	return nil, nil
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	return &p.podMap[types.NamespacedName{Name: name, Namespace: namespace}].Status, nil
}

func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	var podList []*v1.Pod
	for _, pod := range p.podMap {
		podList = append(podList, pod)
	}
	return podList, nil
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, name, container string, cmd []string, attach api.AttachIO) error {
	return nil
}

func (p *Provider) nodeConditions() []v1.NodeCondition {
	// TODO: Make this configurable
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

func (p *Provider) nodeAddresses() []v1.NodeAddress {
	return []v1.NodeAddress{
		{
			Type:    "InternalIP",
			Address: p.internalIP,
		},
	}
}

func (p *Provider) nodeDaemonEndpoints() v1.NodeDaemonEndpoints {
	return v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: p.daemonEndpointPort,
		},
	}
}
