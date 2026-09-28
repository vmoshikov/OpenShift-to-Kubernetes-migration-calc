package main

import (
	"errors"
	"os"
)

// Заготовка под фактическое потребление из Prometheus. НЕ РЕАЛИЗОВАНО:
// сейчас весь расчёт опирается на limits/requests из манифестов, т.е. на то,
// что команды когда-то вписали, а не на то, что сервисы реально потребляют.
// Ниже — контракт и запросы, которые нужно доработать.
//
// Откуда брать на OpenShift: встроенный мониторинг кластера, эндпоинт
// thanos-querier (route в namespace openshift-monitoring), авторизация
// bearer-токеном: `oc whoami -t` или serviceaccount с ролью cluster-monitoring-view.
//
// Окно — не меньше 14 дней (лучше 30), чтобы захватить недельную
// сезонность и месячные пики (закрытие периода, отчёты, релизы).

// UsageStats — фактическое потребление одного контейнера за окно.
type UsageStats struct {
	CPUP95Milli    int64   `json:"cpu_p95_milli"`
	CPUP99Milli    int64   `json:"cpu_p99_milli"`
	MemP99MiB      int64   `json:"mem_p99_mib"` // working set, не RSS/cache
	MemMaxMiB      int64   `json:"mem_max_mib"`
	ThrottledRatio float64 `json:"cpu_throttled_ratio"` // доля CFS-периодов с троттлингом
	OOMKills       int     `json:"oom_kills"`
	MaxReplicas    int     `json:"max_replicas"` // для HPA
	Window         string  `json:"window"`
}

// PromQL-шаблоны ($ns — namespace, $win — окно, напр. 14d).
// Агрегация до workload: pod → ReplicaSet → Deployment через kube_pod_owner
// и kube_replicaset_owner (kube-state-metrics); для DC — ReplicationController.
var promQueries = map[string]string{
	"cpu_p95": `quantile_over_time(0.95, sum by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{namespace="$ns", container!="", container!="POD"}[5m]))[$win:5m])`,
	"cpu_p99": `quantile_over_time(0.99, sum by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{namespace="$ns", container!="", container!="POD"}[5m]))[$win:5m])`,
	"mem_p99": `quantile_over_time(0.99, container_memory_working_set_bytes{namespace="$ns", container!="", container!="POD"}[$win])`,
	"mem_max": `max_over_time(container_memory_working_set_bytes{namespace="$ns", container!="", container!="POD"}[$win])`,
	"throttled": `sum by (namespace, pod, container) (increase(container_cpu_cfs_throttled_periods_total{namespace="$ns"}[$win]))
  / sum by (namespace, pod, container) (increase(container_cpu_cfs_periods_total{namespace="$ns"}[$win]))`,
	"oom_kills":    `sum by (namespace, pod, container) (increase(kube_pod_container_status_restarts_total{namespace="$ns"}[$win]) and on (namespace, pod, container) kube_pod_container_status_last_terminated_reason{namespace="$ns", reason="OOMKilled"})`,
	"hpa_replicas": `max_over_time(kube_horizontalpodautoscaler_status_current_replicas{namespace="$ns"}[$win])`,
}

var errPrometheusNotImplemented = errors.New("получение метрик из Prometheus не реализовано (см. prometheus.go)")

// FetchUsage — TODO: выполнить promQueries через /api/v1/query на
// PROMETHEUS_URL (thanos-querier) с токеном PROMETHEUS_TOKEN, свести
// результаты по workload/container и вернуть map["ns/name/container"].
//
// Как использовать результат (точки встраивания помечены TODO(prometheus)):
//   - базис V: max(p95 CPU × запас, requests) вместо limits — или limits,
//     но с предупреждением, если limits > p99 × 3 («воздух»);
//   - override: новый limit не ниже p99 × 1.2 (память — от max, не p99:
//     память не троттлится, превышение = OOMKill);
//   - CPU: при throttled_ratio > 5% не сжимать CPU limit, даже если ratio велик;
//   - HPA: реплики из max_over_time, а не из spec.replicas дампа.
func FetchUsage(namespace, window string) (map[string]UsageStats, error) {
	_ = os.Getenv("PROMETHEUS_URL")
	_ = os.Getenv("PROMETHEUS_TOKEN")
	return nil, errPrometheusNotImplemented
}
