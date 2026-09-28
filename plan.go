package main

import (
	"fmt"
	"math"
)

// План по одному деплойменту в целевом контуре — общий источник правды для
// sizing (суммы), генерации манифестов (конкретные limits/replicas) и
// JSON-отчёта (детализация для агента).

// ContainerPlan — ресурсы контейнера в целевом контуре.
type ContainerPlan struct {
	Name          string       `json:"name"`
	Requests      ResourceSpec `json:"requests"`
	Limits        ResourceSpec `json:"limits"`          // после override; если limit не задан — requests (только для базиса)
	OrigLimits    ResourceSpec `json:"original_limits"` // как в исходном стенде (0 — не задан)
	CPUOverridden bool         `json:"cpu_overridden"`
	MemOverridden bool         `json:"mem_overridden"`
	CPUNoLimit    bool         `json:"cpu_no_limit"` // limit не задан, в базис взят requests
	MemNoLimit    bool         `json:"mem_no_limit"`
	// TODO(prometheus): сюда же — фактическое потребление p95/p99 (UsageStats)
	// и рекомендованный по нему limit, чтобы override опирался на нагрузку,
	// а не только на соотношение limits/requests.
}

// DeploymentPlan — деплоймент в целевом контуре.
type DeploymentPlan struct {
	Name         string          `json:"name"`
	Namespace    string          `json:"namespace"`
	Kind         string          `json:"kind"`
	FromReplicas int             `json:"source_replicas"`
	Replicas     int             `json:"replicas"`
	Containers   []ContainerPlan `json:"containers"`
	src          *DeploymentSpec // исходник (raw-манифест) для генерации
}

// planReplicas пересчитывает реплики со стенда-источника на целевой контур.
// TODO(prometheus): для деплойментов с HPA брать не spec.replicas, а
// max_over_time(kube_horizontalpodautoscaler_status_current_replicas[14d]) —
// spec.replicas у HPA-управляемых сервисов отражает случайный момент дампа.
func planReplicas(replicas int, source, env EnvProfile) int {
	n := int(math.Round(float64(replicas) * env.ReplicaFactor / source.ReplicaFactor))
	if n < env.MinReplicas {
		n = env.MinReplicas
	}
	return n
}

// PlanDeployment применяет к деплойменту пересчёт реплик и override контура.
func PlanDeployment(d *DeploymentSpec, source, env EnvProfile) DeploymentPlan {
	p := DeploymentPlan{
		Name: d.Name, Namespace: d.Namespace, Kind: d.Kind,
		FromReplicas: d.Replicas,
		Replicas:     planReplicas(d.Replicas, source, env),
		src:          d,
	}
	for _, c := range d.Containers {
		cp := ContainerPlan{Name: c.Name, Requests: c.Requests, OrigLimits: c.Limits, Limits: c.Limits}
		// Limits не задан — в базис идёт requests (иначе контейнер выпал бы из V).
		if cp.Limits.CPUMilli <= 0 {
			cp.Limits.CPUMilli, cp.CPUNoLimit = c.Requests.CPUMilli, true
		}
		if cp.Limits.MemMiB <= 0 {
			cp.Limits.MemMiB, cp.MemNoLimit = c.Requests.MemMiB, true
		}
		if env.Override.Enabled() {
			cp.Limits.CPUMilli, cp.CPUOverridden = env.Override.CPU.apply(c.Requests.CPUMilli, cp.Limits.CPUMilli)
			cp.Limits.MemMiB, cp.MemOverridden = env.Override.Mem.apply(c.Requests.MemMiB, cp.Limits.MemMiB)
		}
		p.Containers = append(p.Containers, cp)
	}
	return p
}

// DataWarning — особенность исходных данных, влияющая на точность расчёта.
type DataWarning struct {
	Deployment string `json:"deployment"`
	Container  string `json:"container,omitempty"`
	Message    string `json:"message"`
}

// AnalyzeDeployments ищет в исходных данных то, что искажает базис
// «сумма Limits» или требует решения человека.
func AnalyzeDeployments(ds []DeploymentSpec) []DataWarning {
	var ws []DataWarning
	for _, d := range ds {
		id := d.Namespace + "/" + d.Name
		if d.Replicas == 0 {
			ws = append(ws, DataWarning{id, "", "replicas=0 (выключен на стенде) — в расчёт попадёт с MinReplicas контура; проверить, нужен ли сервис"})
		}
		if d.Kind == "DeploymentConfig" {
			ws = append(ws, DataWarning{id, "", "DeploymentConfig — при генерации манифеста конвертируется в Deployment; триггеры ImageChange/ConfigChange не переносятся"})
		}
		for _, c := range d.Containers {
			switch {
			case c.Requests.CPUMilli <= 0 && c.Requests.MemMiB <= 0 && c.Limits.CPUMilli <= 0 && c.Limits.MemMiB <= 0:
				// TODO(prometheus): для BestEffort-контейнеров брать p95 CPU / p99 memory как базис.
				ws = append(ws, DataWarning{id, c.Name, "нет ни requests, ни limits (BestEffort) — вклад в V нулевой; нужны данные Prometheus"})
			case c.Limits.CPUMilli <= 0 && c.Limits.MemMiB <= 0:
				ws = append(ws, DataWarning{id, c.Name, "limits не заданы — в базис взяты requests, V может быть занижен"})
			case c.Limits.CPUMilli <= 0:
				ws = append(ws, DataWarning{id, c.Name, "CPU limit не задан — в базис взят CPU request"})
			case c.Limits.MemMiB <= 0:
				ws = append(ws, DataWarning{id, c.Name, "memory limit не задан — в базис взят memory request; риск OOM соседей на ноде"})
			}
			if c.Limits.CPUMilli > 0 && c.Requests.CPUMilli > c.Limits.CPUMilli ||
				c.Limits.MemMiB > 0 && c.Requests.MemMiB > c.Limits.MemMiB {
				ws = append(ws, DataWarning{id, c.Name, "requests > limits — невалидно для K8s, override такой контейнер не трогает; исправить вручную"})
			}
		}
	}
	return ws
}

func (w DataWarning) String() string {
	if w.Container != "" {
		return fmt.Sprintf("[%s/%s] %s", w.Deployment, w.Container, w.Message)
	}
	return fmt.Sprintf("[%s] %s", w.Deployment, w.Message)
}
