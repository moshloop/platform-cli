package monitoring

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/grafana-tools/sdk"
	log "github.com/sirupsen/logrus"

	"github.com/flanksource/commons/text"
	"github.com/moshloop/platform-cli/pkg/k8s"
	"github.com/moshloop/platform-cli/pkg/platform"
)

const (
	Namespace = "monitoring"
)

var specs = []string{"grafana-operator.yml", "kube-prometheus.yml", "prometheus-adapter.yml", "kube-state-metrics.yml", "node-exporter.yml", "alertmanager-rules.yml.raw", "service-monitors.yml"}

func Install(p *platform.Platform) error {
	if p.Monitoring == nil || p.Monitoring.Disabled {
		return nil
	}

	if err := p.CreateOrUpdateNamespace(Namespace, nil, nil); err != nil {
		return err
	}

	if err := p.ApplySpecs("", "monitoring/prometheus-operator.yml"); err != nil {
		log.Warnf("Failed to deploy prometheus operator %v", err)
	}

	data, err := p.Template("monitoring/alertmanager.yaml", "manifests")
	if err != nil {
		return err
	}
	if err := p.CreateOrUpdateSecret("alertmanager-main", Namespace, map[string][]byte{
		"alertmanager.yaml": []byte(data),
	}); err != nil {
		return err
	}

	if p.Thanos == nil || p.Thanos.Disabled {
		log.Println("Thanos is disabled")
	} else {
		s3Client, err := p.GetS3Client()
		if err != nil {
			return err
		}

		exists, err := s3Client.BucketExists(p.Thanos.S3.Bucket)
		if err != nil {
			return err
		}
		if !exists {
			if err := s3Client.MakeBucket(p.Thanos.S3.Bucket, p.S3.Region); err != nil {
				return err
			}
		}
		if err := p.ApplySpecs("", "monitoring/thanosConfig.yaml"); err != nil {
			return err
		}
	}
	if p.Thanos.Mode == "client" {
		log.Info("Thanos in client mode is enabled. Sidecar will be deployed within Promerheus pod.")
		p.ApplySpecs("", "monitoring/thanosSidecar.yaml")
	} else if p.Thanos.Mode == "observability" {
		log.Info("Thanos in observability mode is enabled. Compactor, Querier and Store will be deployed.")
		if p.Thanos.ThanosSidecarEndpoint == "" || p.Thanos.ThanosSidecarPort == "" {
			return errors.New("thanosSidecarEndpoint and thanosSidecarPort should not be empty")
		}
		thanosSpecs := []string{"thanosCompactor.yaml",  "thanosQuerier.yaml", "thanosStore.yaml"}
		for _, spec := range thanosSpecs {
			log.Infof("Applying %s", spec)
			if err := p.ApplySpecs("", "monitoring/observability/"+spec); err != nil {
				return err
			}
		}
	} else {
		log.Printf("Thanos: wrong mode %s. Should be client or observability", p.Thanos.Mode)
	}

	for _, spec := range specs {
		log.Infof("Applying %s", spec)
		if err := p.ApplySpecs("", "monitoring/"+spec); err != nil {
			return err
		}
	}

	dashboards, err := p.GetResourcesByDir("/monitoring/dashboards", "manifests")
	if err != nil {
		return fmt.Errorf("Unable to find dashboards: %v", err)
	}

	urls := map[string]string {
		"alertmanager": fmt.Sprintf("https://alertmanager.%s", p.Domain),
		"grafana": fmt.Sprintf("https://grafana.%s", p.Domain),
		"prometheus": fmt.Sprintf("https://prometheus.%s", p.Domain),
	}

	for name, file := range dashboards {
		contents := text.SafeRead(file)
		var board sdk.Board
		if err := json.Unmarshal([]byte(contents), &board); err != nil {
			log.Warnf("Invalid grafana dashboard %s: %v", name, err)
		}

		for i := range board.Templating.List {
			for k, v := range urls {
				if k == board.Templating.List[i].Name {
					board.Templating.List[i].Current.Value = v
					board.Templating.List[i].Current.Text = v
					board.Templating.List[i].Query = v
				}
			}
		}

		contentsModified, err := json.Marshal(&board)
		if err != nil {
			log.Warnf("Failed to marshal dashboard json %s: %v", name, err)
		}

		if err := p.ApplyCRD("monitoring", k8s.CRD{
			ApiVersion: "integreatly.org/v1alpha1",
			Kind:       "GrafanaDashboard",
			Metadata: k8s.Metadata{
				Name:      name,
				Namespace: Namespace,
				Labels: map[string]string{
					"app": "grafana",
				},
			},
			Spec: map[string]interface{}{
				"name": name,
				"json": string(contentsModified),
			},
		}); err != nil {
			return err
		}
	}

	return nil
}
