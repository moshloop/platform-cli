package phases

import (
	"errors"
	"fmt"
	// initialize konfigadm

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"github.com/moshloop/commons/certs"
	_ "github.com/moshloop/konfigadm/pkg"
	konfigadm "github.com/moshloop/konfigadm/pkg/types"
	"github.com/moshloop/platform-cli/pkg/platform"
)

var envVars = map[string]string{
	"ETCDCTL_ENDPOINTS": "https://127.0.0.1:2379",
	"ETCDCTL_CACERT":    "/etc/kubernetes/pki/etcd/ca.crt",
	"ETCDCTL_CERT":      "/etc/kubernetes/pki/etcd/healthcheck-client.crt",
	"ETCDCTL_KEY":       "/etc/kubernetes/pki/etcd/healthcheck-client.key",
	"KUBECONFIG":        "/etc/kubernetes/admin.conf",
}

func CreatePrimaryMaster(platform *platform.Platform) (*konfigadm.Config, error) {
	if platform.Name == "" {
		return nil, errors.New("Must specify a platform name")
	}
	if platform.Datacenter == "" {
		return nil, errors.New("Must specify a platform datacenter")
	}

	log.Infof("Creating new primary master\n")
	hostname := ""
	cfg, err := baseKonfig(platform)
	if err != nil {
		return nil, err
	}
	if err := addInitKubeadmConfig(hostname, platform, cfg); err != nil {
		return nil, err
	}
	createConsulService(hostname, platform, cfg)
	createClientSideLoadbalancers(platform, cfg)
	addCerts(platform, cfg)
	cfg.AddCommand("kubeadm init --upload-certs --config /etc/kubernetes/kubeadm.conf > /var/log/kubeadm.log")
	return cfg, nil
}

func baseKonfig(platform *platform.Platform) (*konfigadm.Config, error) {
	platform.Init()
	cfg, err := konfigadm.NewConfig().Build()
	if err != nil {
		return nil, err
	}
	for k, v := range envVars {
		cfg.Environment[k] = v
	}

	// update hosts file with hostname
	cfg.AddCommand("echo $(ifconfig ens160 | grep inet | awk '{print $2}' | head -n1 ) $(hostname) >> /etc/hosts")
	return cfg, nil
}

func addCerts(platform *platform.Platform, cfg *konfigadm.Config) error {
	clusterCA := certs.NewCertificateBuilder("kubernetes-ca").CA().Certificate

	// any cert signed by the global CA should be allowed
	crt := string(platform.GetCA().GetPublicChain()[0].EncodedCertificate()) + "\n"
	// plus any cert signed by this cluster specific CA
	crt += string(clusterCA.EncodedCertificate())
	cfg.Files["/etc/kubernetes/pki/ca.crt"] = crt
	cfg.Files["/etc/kubernetes/pki/ca.key"] = string(clusterCA.EncodedPrivateKey())
	cfg.Files["/etc/ssl/certs/openid-ca.pem"] = string(platform.GetIngressCA().GetPublicChain()[0].EncodedCertificate())
	return nil
}

func addInitKubeadmConfig(hostname string, platform *platform.Platform, cfg *konfigadm.Config) error {
	cluster := NewClusterConfig(platform)
	data, err := yaml.Marshal(cluster)
	if err != nil {
		return err
	}
	cfg.Files["/etc/kubernetes/kubeadm.conf"] = string(data)
	return nil
}

func createConsulService(hostname string, platform *platform.Platform, cfg *konfigadm.Config) {
	cfg.Files["/etc/kubernetes/consul/api.json"] = fmt.Sprintf(`
{
	"leave_on_terminate": true,
  "rejoin_after_leave": true,
	"service": {
		"id": "%s",
		"name": "%s",
		"address": "",
		"check": {
			"id": "api-server",
			"name": " TCP on port 6443",
			"tcp": "localhost:6443",
			"interval": "120s",
			"timeout": "60s"
		},
		"port": 6443,
		"enable_tag_override": false
	}
}
	`, hostname, platform.Name)
}

func createClientSideLoadbalancers(platform *platform.Platform, cfg *konfigadm.Config) {
	cfg.Containers = append(cfg.Containers, konfigadm.Container{
		Image: platform.GetImagePath("docker.io/consul:1.3.1"),
		Env: map[string]string{
			"CONSUL_CLIENT_INTERFACE": "ens160",
			"CONSUL_BIND_INTERFACE":   "ens160",
		},
		Args:       fmt.Sprintf("agent -join=%s:8301 -datacenter=%s -data-dir=/consul/data -domain=consul -config-dir=/consul-configs", platform.Consul, platform.Datacenter),
		DockerOpts: "--net host",
		Volumes: []string{
			"/etc/kubernetes/consul:/consul-configs",
		},
	}, konfigadm.Container{
		Image:      platform.GetImagePath("docker.io/moshloop/tcp-loadbalancer:0.1"),
		Service:    "haproxy",
		DockerOpts: "--net host -p 8443:8443",
		Env: map[string]string{
			"CONSUL_CONNECT": platform.Consul + ":8500",
			"SERVICE_NAME":   platform.Name,
			"PORT":           "8443",
		},
	})
}

func getOrCreateBootstrapToken(platform *platform.Platform) (string, error) {
	if platform.BootstrapToken != "" {
		return platform.BootstrapToken, nil
	}
	client, err := platform.GetClientset()
	if err != nil {
		return "", err
	}
	token, err := createBootstrapToken(client.CoreV1().Secrets("kube-system"))
	if err != nil {
		return "", err
	}
	platform.BootstrapToken = token

	return platform.BootstrapToken, nil
}

func CreateSecondaryMaster(platform *platform.Platform) (*konfigadm.Config, error) {
	hostname := ""
	cfg, err := baseKonfig(platform)
	if err != nil {
		return nil, err
	}
	token, err := getOrCreateBootstrapToken(platform)
	if err != nil {
		return nil, err
	}
	createConsulService(hostname, platform, cfg)
	createClientSideLoadbalancers(platform, cfg)
	addCerts(platform, cfg)
	cfg.AddCommand(fmt.Sprintf(
		"kubeadm join --control-plane --token %s --discovery-token-unsafe-skip-ca-verification %s  > /var/log/kubeadm.log",
		token, platform.JoinEndpoint))
	return cfg, nil
}

func CreateWorker(platform *platform.Platform) (*konfigadm.Config, error) {
	cfg, err := baseKonfig(platform)
	if err != nil {
		return nil, err
	}
	token, err := getOrCreateBootstrapToken(platform)
	if err != nil {
		return nil, err
	}
	createClientSideLoadbalancers(platform, cfg)
	cfg.AddCommand(fmt.Sprintf(
		"kubeadm join --token %s --discovery-token-unsafe-skip-ca-verification %s > /var/log/kubeadm.log",
		token, platform.JoinEndpoint))
	return cfg, nil
}
