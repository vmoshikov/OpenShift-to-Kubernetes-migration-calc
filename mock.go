package main

// MockDeployments эмулирует "10 деплойментов по 10 подов" на OpenShift-стенде.
// Профиль ресурсов намеренно неоднородный (как в реальном коммунальном кластере):
// часть сервисов лёгкие (API/воркеры), часть — тяжёлые (кэш, БД-прокси, индексация),
// чтобы демонстрация капасити-калькулятора не сводилась к тривиальному умножению.
func MockDeployments() []DeploymentSpec {
	type tpl struct {
		name        string
		replicas    int
		cpuReq      int64 // millicores
		memReq      int64 // MiB
		cpuLimitMul float64
		memLimitMul float64
	}

	templates := []tpl{
		{"api-gateway", 10, 250, 256, 2.0, 1.5},
		{"auth-service", 10, 200, 384, 2.0, 1.5},
		{"catalog-service", 10, 300, 512, 2.0, 1.5},
		{"order-service", 10, 300, 512, 2.0, 1.5},
		{"notification-worker", 10, 150, 256, 2.0, 1.5},
		{"search-indexer", 10, 500, 1024, 1.5, 1.2},
		{"cache-proxy", 10, 400, 768, 1.5, 1.2},
		{"reporting-service", 10, 350, 640, 2.0, 1.5},
		{"media-processor", 10, 600, 1024, 1.3, 1.2},
		{"legacy-billing-adapter", 10, 450, 896, 1.5, 1.3},
	}

	deployments := make([]DeploymentSpec, 0, len(templates))
	for _, t := range templates {
		container := ContainerSpec{
			Name:     t.name,
			Requests: ResourceSpec{CPUMilli: t.cpuReq, MemMiB: t.memReq},
			Limits: ResourceSpec{
				CPUMilli: int64(float64(t.cpuReq) * t.cpuLimitMul),
				MemMiB:   int64(float64(t.memReq) * t.memLimitMul),
			},
		}
		deployments = append(deployments, DeploymentSpec{
			Name:       t.name,
			Namespace:  "demo-client",
			Replicas:   t.replicas,
			Containers: []ContainerSpec{container},
		})
	}
	return deployments
}
