# kube2consul
Add a DNS record for each Kubernetes Service. It will use the service's PortalIP

Heavily inspired by https://github.com/GoogleCloudPlatform/kubernetes/tree/master/cluster/addons/dns/kube2sky

## Usage
./kube2consul --consul-agent http://consul-agent-host:8500 --kube_master_url http://kube-master-host:8080
