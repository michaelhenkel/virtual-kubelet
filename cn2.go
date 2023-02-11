package cn2vk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/virtual-kubelet/node-cli/manager"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type CNIHandler struct {
	PodChan chan PodAction
}

type PodAction struct {
	pod     *v1.Pod
	counter int32
	reply   chan string
	command string
}

func NewUpdateHandler() *CNIHandler {
	return &CNIHandler{
		PodChan: make(chan PodAction),
	}
}

func (uh *CNIHandler) Run(cniPath string) {
	go func() {
		for podAction := range uh.PodChan {
			res, err := runCNI(podAction.pod.Name, podAction.pod.Namespace, podAction.command, cniPath, podAction.counter)
			if err != nil {
				klog.Error(err)

			}
			klog.Info("handled ", podAction.counter)
			podAction.reply <- res
		}
	}()
}

type Action string

const (
	Add  Action = "add"
	Del  Action = "del"
	Get  Action = "get"
	List Action = "list"
)

type DB struct {
	podStatus map[types.UID]string
	podMap    map[types.NamespacedName]*v1.Pod
	PodChan   chan struct {
		types.NamespacedName
		*v1.Pod
		Action
	}
	Reply     chan *v1.Pod
	ReplyList chan []*v1.Pod
	Dir       string
}

func NewDB(initialPodList []*v1.Pod, dir string) *DB {
	var podMap = make(map[types.NamespacedName]*v1.Pod)
	for idx := range initialPodList {
		pod := initialPodList[idx]
		podMap[types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}] = pod
	}

	return &DB{
		podStatus: make(map[types.UID]string),
		podMap:    podMap,
		PodChan: make(chan struct {
			types.NamespacedName
			*v1.Pod
			Action
		}),
		Reply:     make(chan *v1.Pod),
		ReplyList: make(chan []*v1.Pod),
		Dir:       dir,
	}
}

func (d *DB) Run() {
	go func() {

		for pod := range d.PodChan {
			switch pod.Action {
			case Add:
				d.podMap[pod.NamespacedName] = pod.Pod
				podByte, err := json.Marshal(pod.Pod)
				if err != nil {
					panic(err)
				}
				podFilePath := filepath.Join(d.Dir, string(pod.Pod.UID))
				if err := os.WriteFile(podFilePath, podByte, 0666); err != nil {
					panic(err)
				}
			case Del:
				delete(d.podMap, pod.NamespacedName)
				podFilePath := filepath.Join(d.Dir, string(pod.Pod.UID))
				if _, err := os.ReadFile(podFilePath); err != nil {
					if !os.IsNotExist(err) {
						panic(err)
					}
				}
				if err := os.RemoveAll(podFilePath); err != nil {
					panic(err)
				}
			case Get:
				if pod, ok := d.podMap[pod.NamespacedName]; ok {
					d.Reply <- pod
				} else {
					d.Reply <- nil
				}
			case List:
				var podList []*v1.Pod
				for _, pod := range d.podMap {
					podList = append(podList, pod)
				}
				d.ReplyList <- podList
			}
		}

	}()
}

func (d *DB) Add(pod *v1.Pod) {
	d.PodChan <- struct {
		types.NamespacedName
		*v1.Pod
		Action
	}{
		types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		pod,
		Add,
	}
}

func (d *DB) Del(pod *v1.Pod) {
	d.PodChan <- struct {
		types.NamespacedName
		*v1.Pod
		Action
	}{
		types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		pod,
		Del,
	}
}
func (d *DB) Get(namespace, name string) *v1.Pod {
	d.PodChan <- struct {
		types.NamespacedName
		*v1.Pod
		Action
	}{
		types.NamespacedName{Namespace: namespace, Name: name},
		nil,
		Get,
	}
	return <-d.Reply
}

func (d *DB) List() []*v1.Pod {
	d.PodChan <- struct {
		types.NamespacedName
		*v1.Pod
		Action
	}{
		types.NamespacedName{},
		nil,
		List,
	}
	return <-d.ReplyList
}

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
	mut                sync.Mutex
	podMap             map[types.NamespacedName]*v1.Pod
	cniPath            string
	logger             log.Logger
	counter            int32
	createCounter      int32
	replyCounter       int32
	db                 *DB
	uh                 *CNIHandler
	podList            []*v1.Pod
	clientset          *kubernetes.Clientset
}

func NewProvider(nodeName, operatingSystem string, internalIP string, resourceManager *manager.ResourceManager, daemonEndpointPort int32, cniPath string, logger log.Logger, db *DB, uh *CNIHandler, podList []*v1.Pod, clientset *kubernetes.Clientset) (*Provider, error) {

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
		mut:                sync.Mutex{},
		counter:            0,
		createCounter:      0,
		replyCounter:       0,
		db:                 db,
		uh:                 uh,
		podList:            podList,
		clientset:          clientset,
	}

	p := &provider
	return p, nil
}

func (p *Provider) capacity(ctx context.Context) v1.ResourceList {
	var cpuQ resource.Quantity
	cpuQ.Set(int64(80))
	var memQ resource.Quantity
	memQ.Set(int64(15261268))

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
	klog.Info("internal ip ", p.internalIP)
	pod.Status.HostIP = p.internalIP
	pod.Status.Phase = v1.PodPending
	pod.Generation = 1
	pod.CreationTimestamp = metav1.Now()

	p.db.Add(pod)

	fmt.Printf("CREATED %d PODS\n", p.createCounter)
	return nil
}

var cniConf = `{
	"cniVersion": "0.4.0",
	"name": "new",
	"type": "loopback"
}`

func runCNI(podName, podNamespace, command, cniPath string, id int32) (string, error) {

	//ctx := context.Background()
	var cancel context.CancelFunc
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(120)*time.Second)
	defer cancel()

	var i, o, e bytes.Buffer
	if _, err := i.Write([]byte(cniConf)); err != nil {
		return e.String(), err
	}

	cmd := exec.CommandContext(ctx, cniPath)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, fmt.Sprintf("CNI_COMMAND=%s", command))
	cmd.Env = append(cmd.Env, "CNI_CONTAINERID=1")
	cmd.Env = append(cmd.Env, "CNI_NETNS=1")
	cmd.Env = append(cmd.Env, "CNI_IFNAME=eth1")
	cmd.Env = append(cmd.Env, "CNI_PATH=/bin")
	cmd.Env = append(cmd.Env, fmt.Sprintf("CNI_ARGS=K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;ID=%d", podName, podNamespace, id))
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
	//fmt.Println("POD UPDATE")

	existingPod := p.db.Get(pod.Namespace, pod.Name)
	if existingPod != nil && existingPod.Status.PodIP == "" {
		p.mut.Lock()
		p.counter = p.counter + 1
		p.mut.Unlock()
		fmt.Println("1 called cni ", p.counter)

		var reply = make(chan string)
		p.uh.PodChan <- PodAction{
			pod:     pod,
			counter: p.counter,
			reply:   reply,
			command: "ADD",
		}

		res := <-reply
		klog.Info(res)

		p.mut.Lock()
		p.replyCounter = p.replyCounter + 1
		p.mut.Unlock()
		fmt.Println("1 got cni replies", p.replyCounter)
		//time.Sleep(time.Duration(time.Second * 2))

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
		pod.Status.HostIP = p.internalIP
		pod.Status.PodIP = v4ip
		pod.Status.PodIPs = podIps
		pod.Generation = pod.Generation + 1
		pod.Status.Phase = v1.PodRunning
		startTime := metav1.Now()
		pod.Status.StartTime = &startTime
		pod.Status.ContainerStatuses = containerStatusList
		pod.Status.Conditions = []v1.PodCondition{{
			Type:   v1.ContainersReady,
			Status: v1.ConditionTrue,
		}, {
			Type:   v1.PodInitialized,
			Status: v1.ConditionTrue,
		}, {
			Type:   v1.PodReady,
			Status: v1.ConditionTrue,
		}, {
			Type:   v1.PodScheduled,
			Status: v1.ConditionTrue,
		}}
		fmt.Println("2 got cni replies", p.replyCounter)

		fmt.Println("pods:", len(p.db.List()))
	}

	//fmt.Println("Pod/ip: ", pod.Name, v4ip)
	p.db.Add(pod)

	return nil
}
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {

	po := p.db.Get(pod.Namespace, pod.Name)
	if po == nil {
		return nil
	}

	var containerStatusList []v1.ContainerStatus
	for _, container := range pod.Spec.Containers {
		containerStatus := v1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			Ready: true,
			State: v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					FinishedAt: metav1.Now(),
				},
			},
		}
		containerStatusList = append(containerStatusList, containerStatus)
	}
	pod.Status.ContainerStatuses = containerStatusList
	pod.Status.Phase = v1.PodSucceeded
	var reply = make(chan string)
	p.uh.PodChan <- PodAction{
		pod:     pod,
		counter: p.counter,
		reply:   reply,
		command: "DEL",
	}

	res := <-reply
	fmt.Println(res)
	p.mut.Lock()
	p.counter = p.counter - 1
	p.mut.Unlock()
	p.db.Del(pod)

	watcher, err := p.clientset.CoreV1().Pods(pod.Namespace).Watch(context.Background(), metav1.SingleObject(pod.ObjectMeta))
	if err != nil {
		panic(err)
	}
	var stopChan = make(chan bool)
	go func() {
		for w := range watcher.ResultChan() {
			/*
				switch w.Type{
				case watchv1.Added:
				case watchv1.Deleted:
				}
			*/
			p, ok := w.Object.(*v1.Pod)
			if ok {
				if len(p.Finalizers) == 0 {
					watcher.Stop()
					stopChan <- true
				}
			} else {
				watcher.Stop()
				stopChan <- true
			}
		}
	}()
	<-stopChan
	return nil
}
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	m := p.db.Get(namespace, name)
	return m, nil
}

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	return nil, nil
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	p.mut.Lock()
	defer p.mut.Unlock()
	pod := p.db.Get(namespace, name)
	if pod != nil {
		return &pod.Status, nil
	}
	return nil, nil
}

func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	podList := p.db.List()
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
