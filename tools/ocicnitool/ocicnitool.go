package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	cnicurrent "github.com/containernetworking/cni/pkg/types/current"
	"github.com/cri-o/ocicni/pkg/ocicni"
)

const (
	EnvBinDir  = "BIN_PATH"
	EnvConfDir = "CONF_PATH"

	DefaultConfDir = "/etc/cni/net.d"
	DefaultBinDir  = "/opt/cni/bin"

	CmdAdd    = "add"
	CmdStatus = "status"
	CmdDel    = "del"
)

func printSandboxResults(results []cnitypes.Result) {
	for _, r := range results {
		result, _ := cnicurrent.NewResultFromResult(r)
		if result != nil {
			result030, _ := cnicurrent.GetResult(result)
			if result030 != nil {
				for _, ip := range result030.IPs {
					intfDetails := ""
					if ip.Interface != nil && *ip.Interface >= 0 && *ip.Interface < len(result030.Interfaces) {
						intf := result030.Interfaces[*ip.Interface]
						// Only print container sandbox interfaces (not host ones)
						if intf.Sandbox != "" {
							intfDetails = fmt.Sprintf(" (%s %s)", intf.Name, intf.Mac)
						}
					}
					fmt.Fprintf(os.Stdout, "IP: %s%s\n", ip.Address.String(), intfDetails)
				}
			}
		}
	}
}

func main() {
	networksStr := flag.String("networks", "", "comma-separated list of CNI network names (optional)")
	flag.Parse()
	networks := make([]string, 0)
	for _, name := range strings.Split(*networksStr, ",") {
		if len(name) > 0 {
			networks = append(networks, name)
		}
	}

	flag.Usage = func() {
		exe := filepath.Base(os.Args[0])

		fmt.Fprintf(os.Stderr, "%s: Add or remove CNI networks from a network namespace\n", exe)
		fmt.Fprintf(os.Stderr, "  %s [-networks name[,name...]] %s    <pod_namespace> <pod_name> <pod_id> <netns>\n", exe, CmdAdd)
		fmt.Fprintf(os.Stderr, "  %s [-networks name[,name...]] %s <pod_namespace> <pod_name> <pod_id> <netns>\n", exe, CmdStatus)
		fmt.Fprintf(os.Stderr, "  %s [-networks name[,name...]] %s   <pod_namespace> <pod_name> <pod_id> <netns>\n", exe, CmdDel)
	}

	if len(flag.Args()) < 5 {
		flag.Usage()
		return
	}

	confdir := os.Getenv(EnvConfDir)
	if confdir == "" {
		confdir = DefaultConfDir
	}
	bindir := os.Getenv(EnvBinDir)
	if bindir == "" {
		bindir = DefaultBinDir
	}

	plugin, err := ocicni.InitCNI("", confdir, bindir)
	if err != nil {
		exit(err)
	}

	podNetwork := ocicni.PodNetwork{
		Namespace: flag.Args()[1],
		Name:      flag.Args()[2],
		ID:        flag.Args()[3],
		NetNS:     flag.Args()[4],
		Networks:  networks,
	}

	switch flag.Args()[0] {
	case CmdAdd:
		results, err := plugin.SetUpPod(podNetwork)
		if err == nil {
			printSandboxResults(results)
		}
		exit(err)
	case CmdStatus:
		results, err := plugin.GetPodNetworkStatus(podNetwork)
		if err == nil {
			printSandboxResults(results)
		}
		exit(err)
	case CmdDel:
		exit(plugin.TearDownPod(podNetwork))
	}
}

func exit(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
