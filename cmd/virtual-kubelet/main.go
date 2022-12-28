package main

import (
	"context"
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
)

var (
	buildVersion = "N/A"
	buildTime    = "N/A"
	k8sVersion   = "v1.15.2" // This should follow the version of k8s.io/kubernetes we are importing
)

func main() {
	var internalIP, cniPath string
	flags := pflag.NewFlagSet("client", pflag.ContinueOnError)
	flags.StringVar(&internalIP, "internal-ip", "10.233.67.4", "node ip.")
	flags.StringVar(&cniPath, "cni-path", "/tmp/cni", "path to cni binary.")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = cli.ContextWithCancelOnSignal(ctx)

	logger := logrus.StandardLogger()
	log.L = logruslogger.FromLogrus(logrus.NewEntry(logger))
	logConfig := &logruscli.Config{LogLevel: "info"}

	trace.T = opencensus.Adapter{}
	traceConfig := opencensuscli.Config{}

	db := cn2vk.NewDB()
	uh := cn2vk.NewUpdateHandler()
	o := opts.New()
	o.Provider = "cn2"
	o.Version = strings.Join([]string{k8sVersion, "vk-cn2", buildVersion}, "-")
	node, err := cli.New(ctx,
		cli.WithBaseOpts(o),
		cli.WithCLIVersion(buildVersion, buildTime),
		cli.WithProvider("cn2", func(cfg provider.InitConfig) (provider.Provider, error) {
			return cn2vk.NewProvider(cfg.NodeName, cfg.OperatingSystem, internalIP, cfg.ResourceManager, cfg.DaemonPort, cniPath, log.G(ctx), db, uh)
		}),
		cli.WithPersistentFlags(logConfig.FlagSet()),
		cli.WithPersistentPreRunCallback(func() error {
			return logruscli.Configure(logConfig, logger)
		}),
		cli.WithPersistentFlags(traceConfig.FlagSet()),
		cli.WithPersistentPreRunCallback(func() error {
			return opencensuscli.Configure(ctx, &traceConfig, o)
		}),
	)
	if err != nil {
		log.G(ctx).Fatal(err)
	}

	go db.Run()
	go uh.Run()

	if err := node.Run(ctx); err != nil {
		log.G(ctx).Fatal(err)
	}
}
