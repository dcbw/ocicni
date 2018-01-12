package ocicni

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	cnicurrent "github.com/containernetworking/cni/pkg/types/current"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
)

type cniNetworkPlugin struct {
	loNetwork *cniNetwork

	sync.RWMutex
	defaultNetName string
	networks       map[string]*cniNetwork

	nsenterPath        string
	pluginDir          string
	cniDirs            []string
	vendorCNIDirPrefix string

	monitorNetDirChan chan struct{}

	// The pod map provides synchronization for a given pod's network
	// operations.  Each pod's setup/teardown/status operations
	// are synchronized against each other, but network operations of other
	// pods can proceed in parallel.
	podsLock sync.Mutex
	pods     map[string]*podLock
}

type cniNetwork struct {
	name          string
	NetworkConfig *libcni.NetworkConfigList
	CNIConfig     libcni.CNI
}

var errMissingDefaultNetwork = errors.New("Missing CNI default network")

type podLock struct {
	// Count of in-flight operations for this pod; when this reaches zero
	// the lock can be removed from the pod map
	refcount uint

	// Lock to synchronize operations for this specific pod
	mu sync.Mutex
}

func buildFullPodName(podNetwork PodNetwork) string {
	return podNetwork.Namespace + "_" + podNetwork.Name
}

// Lock network operations for a specific pod.  If that pod is not yet in
// the pod map, it will be added.  The reference count for the pod will
// be increased.
func (plugin *cniNetworkPlugin) podLock(podNetwork PodNetwork) *sync.Mutex {
	plugin.podsLock.Lock()
	defer plugin.podsLock.Unlock()

	fullPodName := buildFullPodName(podNetwork)
	lock, ok := plugin.pods[fullPodName]
	if !ok {
		lock = &podLock{}
		plugin.pods[fullPodName] = lock
	}
	lock.refcount++
	return &lock.mu
}

// Unlock network operations for a specific pod.  The reference count for the
// pod will be decreased.  If the reference count reaches zero, the pod will be
// removed from the pod map.
func (plugin *cniNetworkPlugin) podUnlock(podNetwork PodNetwork) {
	plugin.podsLock.Lock()
	defer plugin.podsLock.Unlock()

	fullPodName := buildFullPodName(podNetwork)
	lock, ok := plugin.pods[fullPodName]
	if !ok {
		logrus.Warningf("Unbalanced pod lock unref for %s", fullPodName)
		return
	} else if lock.refcount == 0 {
		// This should never ever happen, but handle it anyway
		delete(plugin.pods, fullPodName)
		logrus.Errorf("Pod lock for %s still in map with zero refcount", fullPodName)
		return
	}
	lock.refcount--
	lock.mu.Unlock()
	if lock.refcount == 0 {
		delete(plugin.pods, fullPodName)
	}
}

func (plugin *cniNetworkPlugin) monitorNetDir() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logrus.Errorf("could not create new watcher %v", err)
		return
	}
	defer watcher.Close()

	go func() {
		for {
			select {
			case event := <-watcher.Events:
				logrus.Debugf("CNI monitoring event %v", event)
				if event.Op&fsnotify.Create != fsnotify.Create &&
					event.Op&fsnotify.Write != fsnotify.Write {
					continue
				}

				if err = plugin.syncNetworkConfig(); err != nil {
					logrus.Errorf("CNI config loading failed, continue monitoring: %v", err)
					continue
				}

				// Stop watching when we have a default network
				if plugin.getDefaultNetwork() != nil {
					logrus.Infof("Found CNI default network; stop watching")
					close(plugin.monitorNetDirChan)
					return
				}

			case err := <-watcher.Errors:
				logrus.Errorf("CNI monitoring error %v", err)
				close(plugin.monitorNetDirChan)
				return
			}
		}
	}()

	if err = watcher.Add(plugin.pluginDir); err != nil {
		logrus.Error(err)
		return
	}

	<-plugin.monitorNetDirChan
}

// InitCNI takes the plugin directory and CNI directories where the CNI config
// files should be searched for.  If no valid CNI configs exist, network requests
// will fail until valid CNI config files are present in the config directory.
// If defaultNetName is not empty, a CNI config with that network name will
// be used as the default CNI network, and container network operations will
// fail until that network config is present and valid.
func InitCNI(defaultNetName string, pluginDir string, cniDirs ...string) (CNIPlugin, error) {
	vendorCNIDirPrefix := ""
	if pluginDir == "" {
		pluginDir = DefaultNetDir
	}
	if len(cniDirs) == 0 {
		cniDirs = []string{DefaultCNIDir}
	}
	plugin := &cniNetworkPlugin{
		defaultNetName:     defaultNetName,
		networks:           make(map[string]*cniNetwork),
		loNetwork:          getLoNetwork(cniDirs, vendorCNIDirPrefix),
		pluginDir:          pluginDir,
		cniDirs:            cniDirs,
		vendorCNIDirPrefix: vendorCNIDirPrefix,
		monitorNetDirChan:  make(chan struct{}),
		pods:               make(map[string]*podLock),
	}

	var err error
	plugin.nsenterPath, err = exec.LookPath("nsenter")
	if err != nil {
		return nil, err
	}

	// Fail loudly if plugin directory doesn't exist, because fsnotify watcher
	// won't be able to watch it.
	if _, err := os.Stat(pluginDir); err != nil {
		return nil, err
	}

	if err := plugin.syncNetworkConfig(); err != nil {
		// We do not have a valid default network, so start the
		// monitoring thread.  Network setup/teardown requests
		// will fail until we have a valid default network.
		go plugin.monitorNetDir()
	}

	return plugin, nil
}

func (plugin *cniNetworkPlugin) loadNetworks() (map[string]*cniNetwork, string, error) {
	files, err := libcni.ConfFiles(plugin.pluginDir, []string{".conf", ".conflist", ".json"})
	switch {
	case err != nil:
		return nil, "", err
	case len(files) == 0:
		return nil, "", errMissingDefaultNetwork
	}

	networks := make(map[string]*cniNetwork)
	defaultNetName := ""

	sort.Strings(files)
	for _, confFile := range files {
		var confList *libcni.NetworkConfigList
		if strings.HasSuffix(confFile, ".conflist") {
			confList, err = libcni.ConfListFromFile(confFile)
			if err != nil {
				logrus.Warningf("Error loading CNI config list file %s: %v", confFile, err)
				continue
			}
		} else {
			conf, err := libcni.ConfFromFile(confFile)
			if err != nil {
				logrus.Warningf("Error loading CNI config file %s: %v", confFile, err)
				continue
			}
			if conf.Network.Type == "" {
				logrus.Warningf("Error loading CNI config file %s: no 'type'; perhaps this is a .conflist?", confFile)
				continue
			}
			confList, err = libcni.ConfListFromConf(conf)
			if err != nil {
				logrus.Warningf("Error converting CNI config file %s to list: %v", confFile, err)
				continue
			}
		}
		if len(confList.Plugins) == 0 {
			logrus.Warningf("CNI config list %s has no networks, skipping", confFile)
			continue
		}
		if confList.Name == "" {
			confList.Name = path.Base(confFile)
		}

		logrus.Infof("Found CNI network %s (type=%v) at %s", confList.Name, confList.Plugins[0].Network.Type, confFile)

		// Search for vendor-specific plugins as well as default plugins in the CNI codebase.
		cninet := &libcni.CNIConfig{
			Path: plugin.cniDirs,
		}
		for _, p := range confList.Plugins {
			vendorDir := vendorCNIDir(plugin.vendorCNIDirPrefix, p.Network.Type)
			cninet.Path = append(cninet.Path, vendorDir)
		}
		networks[confList.Name] = &cniNetwork{
			name:          confList.Name,
			NetworkConfig: confList,
			CNIConfig:     cninet,
		}

		if defaultNetName == "" {
			defaultNetName = confList.Name
		}
	}

	if len(networks) == 0 {
		return nil, "", fmt.Errorf("No valid networks found in %s", plugin.pluginDir)
	}

	return networks, defaultNetName, nil
}

func vendorCNIDir(prefix, pluginType string) string {
	return fmt.Sprintf(VendorCNIDirTemplate, prefix, pluginType)
}

func getLoNetwork(cniDirs []string, vendorDirPrefix string) *cniNetwork {
	loConfig, err := libcni.ConfListFromBytes([]byte(`{
  "cniVersion": "0.2.0",
  "name": "cni-loopback",
  "plugins": [{
    "type": "loopback"
  }]
}`))
	if err != nil {
		// The hardcoded config above should always be valid and unit tests will
		// catch this
		panic(err)
	}
	vendorDir := vendorCNIDir(vendorDirPrefix, loConfig.Plugins[0].Network.Type)
	cninet := &libcni.CNIConfig{
		Path: append(cniDirs, vendorDir),
	}
	loNetwork := &cniNetwork{
		name:          "lo",
		NetworkConfig: loConfig,
		CNIConfig:     cninet,
	}

	return loNetwork
}

func (plugin *cniNetworkPlugin) syncNetworkConfig() error {
	networks, defaultNetName, err := plugin.loadNetworks()
	if err != nil {
		logrus.Errorf("Error loading CNI networks: %s", err)
		return err
	}

	plugin.Lock()
	defer plugin.Unlock()
	if plugin.defaultNetName == "" {
		plugin.defaultNetName = defaultNetName
	}
	plugin.networks = networks

	return nil
}

func (plugin *cniNetworkPlugin) getNetwork(name string) (*cniNetwork, error) {
	plugin.RLock()
	defer plugin.RUnlock()
	net, ok := plugin.networks[name]
	if !ok {
		return nil, fmt.Errorf("CNI network %q not found", name)
	}
	return net, nil
}

func (plugin *cniNetworkPlugin) getDefaultNetworkName() string {
	plugin.RLock()
	defer plugin.RUnlock()
	return plugin.defaultNetName
}

func (plugin *cniNetworkPlugin) getDefaultNetwork() *cniNetwork {
	defaultNetName := plugin.getDefaultNetworkName()
	if defaultNetName == "" {
		return nil
	}
	network, _ := plugin.getNetwork(defaultNetName)
	return network
}

func (plugin *cniNetworkPlugin) checkInitialized(podNetwork PodNetwork) error {
	if len(podNetwork.Networks) == 0 && plugin.getDefaultNetwork() == nil {
		return errors.New("cni config uninitialized")
	}
	return nil
}

func (plugin *cniNetworkPlugin) Name() string {
	return CNIPluginName
}

func (plugin *cniNetworkPlugin) forEachNetwork(podNetwork PodNetwork, forEachFunc func(*cniNetwork, string, PodNetwork) error) error {
	networks := podNetwork.Networks
	if len(networks) == 0 {
		networks = append(networks, plugin.getDefaultNetworkName())
	}
	for i, netName := range networks {
		// Interface names start at "eth0" and count up for each network
		ifName := fmt.Sprintf("eth%d", i)
		network, err := plugin.getNetwork(netName)
		if err != nil {
			logrus.Errorf(err.Error())
			return err
		}
		if err := forEachFunc(network, ifName, podNetwork); err != nil {
			return err
		}
	}
	return nil
}

func (plugin *cniNetworkPlugin) SetUpPod(podNetwork PodNetwork) ([]cnitypes.Result, error) {
	if err := plugin.checkInitialized(podNetwork); err != nil {
		return nil, err
	}

	plugin.podLock(podNetwork).Lock()
	defer plugin.podUnlock(podNetwork)

	_, err := plugin.loNetwork.addToNetwork(podNetwork, "lo")
	if err != nil {
		logrus.Errorf("Error while adding to cni lo network: %s", err)
		return nil, err
	}

	results := make([]cnitypes.Result, 0)
	if err := plugin.forEachNetwork(podNetwork, func(network *cniNetwork, ifName string, podNetwork PodNetwork) error {
		result, err := network.addToNetwork(podNetwork, ifName)
		if err != nil {
			logrus.Errorf("Error while adding pod to CNI network %q: %s", network.name, err)
			return err
		}
		results = append(results, result)
		return nil
	}); err != nil {
		return nil, err
	}

	return results, nil
}

func (plugin *cniNetworkPlugin) TearDownPod(podNetwork PodNetwork) error {
	if err := plugin.checkInitialized(podNetwork); err != nil {
		return err
	}

	plugin.podLock(podNetwork).Lock()
	defer plugin.podUnlock(podNetwork)

	return plugin.forEachNetwork(podNetwork, func(network *cniNetwork, ifName string, podNetwork PodNetwork) error {
		if err := network.deleteFromNetwork(podNetwork, ifName); err != nil {
			logrus.Errorf("Error while removing pod from CNI network %q: %s", network.name, err)
			return err
		}
		return nil
	})
}

// GetPodNetworkStatus returns IP addressing and interface details for all
// networks attached to the pod.
func (plugin *cniNetworkPlugin) GetPodNetworkStatus(podNetwork PodNetwork) ([]cnitypes.Result, error) {
	plugin.podLock(podNetwork).Lock()
	defer plugin.podUnlock(podNetwork)

	results := make([]cnitypes.Result, 0)
	if err := plugin.forEachNetwork(podNetwork, func(network *cniNetwork, ifName string, podNetwork PodNetwork) error {
		ip, mac, err := getContainerDetails(plugin.nsenterPath, podNetwork.NetNS, ifName, "-4")
		if err != nil {
			return err
		}

		// Until CNI's GET request lands, construct the Result manually
		results = append(results, &cnicurrent.Result{
			CNIVersion: "0.3.1",
			Interfaces: []*cnicurrent.Interface{
				{
					Name:    ifName,
					Mac:     mac.String(),
					Sandbox: podNetwork.NetNS,
				},
			},
			IPs: []*cnicurrent.IPConfig{
				{
					Version:   "4",
					Interface: cnicurrent.Int(0),
					Address:   *ip,
				},
			},
		})
		return nil
	}); err != nil {
		return nil, err
	}

	return results, nil
}

func (network *cniNetwork) addToNetwork(podNetwork PodNetwork, ifName string) (cnitypes.Result, error) {
	rt, err := buildCNIRuntimeConf(podNetwork, ifName)
	if err != nil {
		logrus.Errorf("Error adding network: %v", err)
		return nil, err
	}

	netconf, cninet := network.NetworkConfig, network.CNIConfig
	logrus.Infof("About to add CNI network %s (type=%v)", netconf.Name, netconf.Plugins[0].Network.Type)
	res, err := cninet.AddNetworkList(netconf, rt)
	if err != nil {
		logrus.Errorf("Error adding network: %v", err)
		return nil, err
	}

	return res, nil
}

func (network *cniNetwork) deleteFromNetwork(podNetwork PodNetwork, ifName string) error {
	rt, err := buildCNIRuntimeConf(podNetwork, ifName)
	if err != nil {
		logrus.Errorf("Error deleting network: %v", err)
		return err
	}

	netconf, cninet := network.NetworkConfig, network.CNIConfig
	logrus.Infof("About to del CNI network %s (type=%v)", netconf.Name, netconf.Plugins[0].Network.Type)
	err = cninet.DelNetworkList(netconf, rt)
	if err != nil {
		logrus.Errorf("Error deleting network: %v", err)
		return err
	}
	return nil
}

func buildCNIRuntimeConf(podNetwork PodNetwork, ifName string) (*libcni.RuntimeConf, error) {
	logrus.Infof("Got pod network %+v", podNetwork)

	rt := &libcni.RuntimeConf{
		ContainerID: podNetwork.ID,
		NetNS:       podNetwork.NetNS,
		IfName:      ifName,
		Args: [][2]string{
			{"IgnoreUnknown", "1"},
			{"K8S_POD_NAMESPACE", podNetwork.Namespace},
			{"K8S_POD_NAME", podNetwork.Name},
			{"K8S_POD_INFRA_CONTAINER_ID", podNetwork.ID},
		},
	}

	if len(podNetwork.PortMappings) == 0 {
		return rt, nil
	}

	rt.CapabilityArgs = map[string]interface{}{
		"portMappings": podNetwork.PortMappings,
	}
	return rt, nil
}

func (plugin *cniNetworkPlugin) Status() error {
	return plugin.checkInitialized(PodNetwork{})
}
