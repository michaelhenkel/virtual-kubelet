package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	cn2vk "github.com/michaelhenkel/virtual-kubelet"
	"github.com/sirupsen/logrus"
	pflag "github.com/spf13/pflag"
	cli "github.com/virtual-kubelet/node-cli"
	logruscli "github.com/virtual-kubelet/node-cli/logrus"
	opencensuscli "github.com/virtual-kubelet/node-cli/opencensus"
	"github.com/virtual-kubelet/node-cli/opts"
	"github.com/virtual-kubelet/node-cli/provider"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	"github.com/virtual-kubelet/virtual-kubelet/trace/opencensus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	buildVersion = "N/A"
	buildTime    = "N/A"
	k8sVersion   = "v1.15.2" // This should follow the version of k8s.io/kubernetes we are importing
)

func main() {
	var internalIP, cniPath, podDir, kubeconfig string
	flags := pflag.NewFlagSet("client", pflag.PanicOnError)
	flags.StringVar(&internalIP, "internal-ip", "10.233.65.19", "node ip.")
	flags.StringVar(&cniPath, "cni-path", "/tmp/cni", "path to cni binary.")
	flags.StringVar(&podDir, "pod-dir", "/var/run/agent-rs/pods", "path to pods.")
	if home := homedir.HomeDir(); home != "" {
		flags.StringVar(&kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		flags.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = cli.ContextWithCancelOnSignal(ctx)

	logger := logrus.StandardLogger()
	log.L = logruslogger.FromLogrus(logrus.NewEntry(logger))
	logConfig := &logruscli.Config{LogLevel: "info"}

	trace.T = opencensus.Adapter{}
	traceConfig := opencensuscli.Config{}

	if _, err := os.Stat(podDir); err != nil {
		if err := os.MkdirAll(podDir, os.ModePerm); err != nil {
			log.G(ctx).Fatal(err)
		}
	}

	podFiles, err := ioutil.ReadDir(podDir)
	if err != nil {
		log.G(ctx).Fatal(err)
	}

	var podList []*v1.Pod
	for _, podFile := range podFiles {
		pod := &v1.Pod{}
		podByte, err := os.ReadFile(filepath.Join(podDir, podFile.Name()))
		if err != nil {
			log.G(ctx).Fatal(err)
		}
		if err := json.Unmarshal(podByte, pod); err != nil {
			log.G(ctx).Fatal(err)
		}
		podList = append(podList, pod)
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		kubeConfig, exists := os.LookupEnv("KUBECONFIG")
		if !exists {
			kubeConfig = kubeconfig
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
		if err != nil {
			panic(err.Error())
		}
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	db := cn2vk.NewDB(podList, podDir)
	uh := cn2vk.NewUpdateHandler()
	o := opts.New()
	o.Provider = "cn2"
	o.Version = strings.Join([]string{k8sVersion, "vk-cn2", buildVersion}, "-")
	node, err := cli.New(ctx,
		cli.WithBaseOpts(o),
		cli.WithCLIVersion(buildVersion, buildTime),
		cli.WithProvider("cn2", func(cfg provider.InitConfig) (provider.Provider, error) {
			return cn2vk.NewProvider(cfg.NodeName, cfg.OperatingSystem, internalIP, cfg.ResourceManager, cfg.DaemonPort, cniPath, log.G(ctx), db, uh, podList, clientset)
		}),
		cli.WithPersistentFlags(logConfig.FlagSet()),
		cli.WithPersistentPreRunCallback(func() error {
			return logruscli.Configure(logConfig, logger)
		}),
		cli.WithPersistentFlags(flags),
		cli.WithPersistentFlags(traceConfig.FlagSet()),
		cli.WithPersistentPreRunCallback(func() error {
			return opencensuscli.Configure(ctx, &traceConfig, o)
		}),
	)
	if err != nil {
		log.G(ctx).Fatal(err)
	}

	go db.Run()
	go uh.Run(cniPath)

	if err := node.Run(ctx); err != nil {
		log.G(ctx).Fatal(err)
	}
}
