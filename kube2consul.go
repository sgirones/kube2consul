/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// kube2consul is a bridge between Kubernetes and Consul.  It watches the
// Kubernetes master for changes in Services and creates new DNS records on the
// consul agent.

package main

import (
	"flag"
	"fmt"
	"strings"
	"net/url"
	"os"
	"time"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	kclient "github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	kclientcmd "github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd"
	kclientcmdapi "github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/controller/framework"
	kcontrollerFramework "github.com/GoogleCloudPlatform/kubernetes/pkg/controller/framework"
	kSelector "github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/golang/glog"
	consulapi "github.com/hashicorp/consul/api"
)

var (
	argConsulAgent         = flag.String("consul-agent", "http://127.0.0.1:8500", "URL to consul agent")
	argKubecfgFile         = flag.String("kubecfg_file", "", "Location of kubecfg file for access to kubernetes service")
	argKubeMasterUrl       = flag.String("kube_master_url", "https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT}", "Url to reach kubernetes master. Env variables in this flag will be expanded.")
)

const (
	// Maximum number of attempts to connect to consul server.
	maxConnectAttempts = 12
	// Resync period for the kube controller loop.
	resyncPeriod = 5 * time.Second
)

type kube2consul struct {
	// Consul client.
	consulClient *consulapi.Client
	// DNS domain name.
	domain string
}

func (ks *kube2consul) removeDNS(record string) error {
	glog.V(2).Infof("Removing %s from DNS", record)
	return ks.consulClient.Agent().ServiceDeregister(record)
}

func (ks *kube2consul) addDNS(record string, service *kapi.Service) error {
	if strings.Contains(record, ".") {
		glog.V(1).Infof("Service names containing '.' are not supported: %s\n", service.Name)
		return nil
	}

	// if PortalIP is not set, do not create a DNS records
	if !kapi.IsServiceIPSet(service) {
		glog.V(1).Infof("Skipping dns record for headless service: %s\n", service.Name)
		return nil
	}

	for i := range service.Spec.Ports {
		asr := &consulapi.AgentServiceRegistration{
			ID:			 record,
			Name: 	 record,
			Address: service.Spec.PortalIP,
			Port:    service.Spec.Ports[0].Port,
		}

		glog.V(2).Infof("Setting DNS record: %v -> %v:%d\n", record, service.Spec.PortalIP, service.Spec.Ports[i].Port)
		if err := ks.consulClient.Agent().ServiceRegister(asr); err != nil {
			return err
		}
	}
	return nil
}

func newConsulClient(consulAgent string) (*consulapi.Client, error) {
	var (
		client *consulapi.Client
		err    error
	)

	consulConfig := consulapi.DefaultConfig()
	consulAgentUrl, err := url.Parse(consulAgent)
	if err != nil {
			glog.Infof("Error parsing Consul url")
			return nil, err
	}

	if consulAgentUrl.Host != "" {
	  consulConfig.Address = consulAgentUrl.Host
	}

	if consulAgentUrl.Scheme != "" {
		consulConfig.Scheme = consulAgentUrl.Scheme
	}

	client, err = consulapi.NewClient(consulConfig)
	if err != nil {
			glog.Infof("Error creating Consul client")
			return nil, err
	}

	for attempt := 1; attempt <= maxConnectAttempts; attempt++ {
		if _, err = client.Agent().Self(); err == nil {
			break
		}

		if attempt == maxConnectAttempts {
			break
		}

		glog.Infof("[Attempt: %d] Attempting access to Consul after 5 second sleep", attempt)
		time.Sleep(5 * time.Second)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to connect to Consul agent: %v, error: %v", consulAgent, err)
	}
	glog.Infof("Consul agent found: %v", consulAgent)

	return client, nil
}

func getKubeMasterUrl() (string, error) {
	if *argKubeMasterUrl == "" {
		return "", fmt.Errorf("no --kube_master_url specified")
	}
	parsedUrl, err := url.Parse(os.ExpandEnv(*argKubeMasterUrl))
	if err != nil {
		return "", fmt.Errorf("failed to parse --kube_master_url %s - %v", *argKubeMasterUrl, err)
	}
	if parsedUrl.Scheme == "" || parsedUrl.Host == "" || parsedUrl.Host == ":" {
		return "", fmt.Errorf("invalid --kube_master_url specified %s", *argKubeMasterUrl)
	}
	return parsedUrl.String(), nil
}

// TODO: evaluate using pkg/client/clientcmd
func newKubeClient() (*kclient.Client, error) {
	var config *kclient.Config
	masterUrl, err := getKubeMasterUrl()
	if err != nil {
		return nil, err
	}
	if *argKubecfgFile == "" {
		config = &kclient.Config{
			Host:    masterUrl,
			Version: "v1",
		}
	} else {
		var err error
		if config, err = kclientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&kclientcmd.ClientConfigLoadingRules{ExplicitPath: *argKubecfgFile},
			&kclientcmd.ConfigOverrides{ClusterInfo: kclientcmdapi.Cluster{Server: masterUrl, InsecureSkipTLSVerify: true}}).ClientConfig(); err != nil {
			return nil, err
		}
	}
	glog.Infof("Using %s for kubernetes master", config.Host)
	glog.Infof("Using kubernetes API %s", config.Version)
	return kclient.New(config)
}

func buildNameString(service, namespace string) string {
	return fmt.Sprintf("%s.%s", service, namespace)
}

// Returns a cache.ListWatch that gets all changes to services.
func createServiceLW(kubeClient *kclient.Client) *cache.ListWatch {
	return cache.NewListWatchFromClient(kubeClient, "services", kapi.NamespaceAll, kSelector.Everything())
}

func (ks *kube2consul) newService(obj interface{}) {
	if s, ok := obj.(*kapi.Service); ok {
		name := buildNameString(s.Name, s.Namespace)
		if err := ks.addDNS(s.Name, s); err != nil {
			glog.V(1).Infof("Failed to add service: %v due to: %v", name, err)
		}
	}
}

func (ks *kube2consul) removeService(obj interface{}) {
	if s, ok := obj.(*kapi.Service); ok {
		name := buildNameString(s.Name, s.Namespace)
		if err := ks.removeDNS(s.Name); err != nil {
			glog.V(1).Infof("Failed to remove service: %v due to: %v", name, err)
		}
	}
}

func watchForServices(kubeClient *kclient.Client, ks *kube2consul) {
	var serviceController *kcontrollerFramework.Controller
	_, serviceController = framework.NewInformer(
		createServiceLW(kubeClient),
		&kapi.Service{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    ks.newService,
			DeleteFunc: ks.removeService,
			UpdateFunc: func(oldObj, newObj interface{}) {
				ks.newService(newObj)
			},
		},
	)
	serviceController.Run(util.NeverStop)
}

func main() {
	flag.Parse()
	var err error
	// TODO: Validate input flags.
	ks := kube2consul{}

	if ks.consulClient, err = newConsulClient(*argConsulAgent); err != nil {
		glog.Fatalf("Failed to create Consul client - %v", err)
	}

	kubeClient, err := newKubeClient()
	if err != nil {
		glog.Fatalf("Failed to create a kubernetes client: %v", err)
	}

	watchForServices(kubeClient, &ks)
}
