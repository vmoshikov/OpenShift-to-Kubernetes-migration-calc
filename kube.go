package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Минимальная проекция Deployment API-объекта — нам нужны только
// имя/namespace/replicas и resources.requests|limits каждого контейнера.
// Полный client-go не подключаем намеренно: демо должно собираться офлайн,
// без доступа к proxy.golang.org.

type k8sQuantity string

// toMilliCPU парсит строки вида "250m", "1", "0.5" в миллиядра.
func (q k8sQuantity) toMilliCPU() int64 {
	s := string(q)
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "m") {
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		return v
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f * 1000)
}

// toMiB парсит строки вида "512Mi", "1Gi", "1G" в MiB (грубая, но достаточная для демо оценка).
func (q k8sQuantity) toMiB() int64 {
	s := string(q)
	switch {
	case strings.HasSuffix(s, "Ki"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "Ki"), 64)
		return int64(v / 1024)
	case strings.HasSuffix(s, "Mi"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "Mi"), 64)
		return int64(v)
	case strings.HasSuffix(s, "Gi"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "Gi"), 64)
		return int64(v * 1024)
	case strings.HasSuffix(s, "G"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "G"), 64)
		return int64(v * 1000 * 1000 * 1000 / 1024 / 1024)
	case strings.HasSuffix(s, "M"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "M"), 64)
		return int64(v * 1000 * 1000 / 1024 / 1024)
	default:
		v, _ := strconv.ParseFloat(s, 64)
		return int64(v / (1024 * 1024))
	}
}

type k8sResourceList struct {
	CPU    k8sQuantity `json:"cpu"`
	Memory k8sQuantity `json:"memory"`
}

type k8sContainer struct {
	Name      string `json:"name"`
	Resources struct {
		Requests k8sResourceList `json:"requests"`
		Limits   k8sResourceList `json:"limits"`
	} `json:"resources"`
}

type k8sDeploymentItem struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Replicas int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []k8sContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type k8sDeploymentList struct {
	Items []k8sDeploymentItem `json:"items"`
}

// FetchDeploymentsFromCluster вызывает `oc`/`kubectl get deployments -A -o json`
// и превращает результат во внутреннюю модель. Это демонстрационный путь —
// он читает состояние стенда напрямую, без промежуточного экспорта.
func FetchDeploymentsFromCluster(binary, namespace string) ([]DeploymentSpec, error) {
	var args []string
	if namespace == "" || namespace == "all" {
		args = []string{"get", "deployments", "-A", "-o", "json"}
	} else {
		args = []string{"get", "deployments", "-n", namespace, "-o", "json"}
	}

	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("не удалось выполнить %s %v: %w\n%s", binary, args, err, string(out))
	}

	deployments, err := parseDeploymentListJSON(out)
	if err != nil {
		return nil, fmt.Errorf("не удалось распарсить вывод %s: %w", binary, err)
	}
	return deployments, nil
}

// parseDeploymentListJSON превращает JSON в формате `kubectl/oc get deployments -o json`
// во внутреннюю модель. Общий путь для живого кластера (FetchDeploymentsFromCluster)
// и для ручной вставки такого же дампа через stdin (ReadDeploymentsFromStdinJSON).
func parseDeploymentListJSON(data []byte) ([]DeploymentSpec, error) {
	var list k8sDeploymentList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}

	deployments := make([]DeploymentSpec, 0, len(list.Items))
	for _, item := range list.Items {
		containers := make([]ContainerSpec, 0, len(item.Spec.Template.Spec.Containers))
		for _, c := range item.Spec.Template.Spec.Containers {
			containers = append(containers, ContainerSpec{
				Name: c.Name,
				Requests: ResourceSpec{
					CPUMilli: c.Resources.Requests.CPU.toMilliCPU(),
					MemMiB:   c.Resources.Requests.Memory.toMiB(),
				},
				Limits: ResourceSpec{
					CPUMilli: c.Resources.Limits.CPU.toMilliCPU(),
					MemMiB:   c.Resources.Limits.Memory.toMiB(),
				},
			})
		}
		replicas := item.Spec.Replicas
		if replicas == 0 {
			replicas = 1
		}
		deployments = append(deployments, DeploymentSpec{
			Name:       item.Metadata.Name,
			Namespace:  item.Metadata.Namespace,
			Replicas:   replicas,
			Containers: containers,
		})
	}
	return deployments, nil
}
