package main

import (
	"fmt"
	"strings"
)

// ResourceSpec хранит запрошенные ресурсы в базовых единицах:
// CPU — в миллиядрах (1000 = 1 vCPU), Mem — в MiB.
type ResourceSpec struct {
	CPUMilli int64
	MemMiB   int64
}

// ContainerSpec — один контейнер пода с requests/limits.
type ContainerSpec struct {
	Name     string
	Requests ResourceSpec
	Limits   ResourceSpec
}

// DeploymentSpec — упрощённая проекция OpenShift DeploymentConfig / K8s Deployment,
// достаточная для расчёта capacity. Все поды деплоймента считаются идентичными
// (типичный случай — один pod template на реплики).
type DeploymentSpec struct {
	Name       string
	Namespace  string
	Replicas   int
	Containers []ContainerSpec
}

// TotalRequests суммирует requests всех контейнеров одного пода этого деплоймента.
func (d DeploymentSpec) PerPodRequests() ResourceSpec {
	var r ResourceSpec
	for _, c := range d.Containers {
		r.CPUMilli += c.Requests.CPUMilli
		r.MemMiB += c.Requests.MemMiB
	}
	return r
}

// NodeFlavor — типоразмер узла ("барик") в целевом Mk8s.
type NodeFlavor struct {
	Name         string
	CPUMilli     int64   // общая ёмкость vCPU в миллиядрах
	MemMiB       int64   // общая ёмкость памяти в MiB
	RelativeCost float64 // условная стоимость часа для сравнения вариантов sizing
	// (только для внутреннего выбора лучшего флейвора/пула — наружу выводится
	// только % оптимизации, а не абсолютные суммы).
}

// EnvProfile описывает, как сущности OpenShift-стенда пересчитываются
// при эмуляции конкретного контура (DEV/IFT/ПСИ/ПРОМ).
type EnvProfile struct {
	Key              string  // dev | ift | psi | prom
	Label            string  // человекочитаемое имя
	ReplicaFactor    float64 // доля от prod-реплик (например 0.2 для DEV)
	MinReplicas      int     // минимум реплик на деплоймент в этом контуре
	CPUOvercommit    float64 // допустимый overcommit CPU на узле (1.0 = без overcommit)
	HeadroomPercent  float64 // запас поверх суммарного спроса (HA/failover/рост)
	SystemReserveCPU int64   // резерв на kubelet/system daemonsets, millicores на узел
	SystemReserveMem int64   // резерв на kubelet/system daemonsets, MiB на узел
}

// NodePoolAlloc — часть решения по sizing: сколько узлов одного флейвора
// взять в пул. Итоговое решение может содержать несколько таких записей
// (смешанный пул узлов), а не только один однородный флейвор.
type NodePoolAlloc struct {
	Flavor NodeFlavor
	Count  int
}

// EnvSizingResult — результат расчёта для одного контура.
type EnvSizingResult struct {
	Env                 EnvProfile
	TotalPods           int
	DemandCPUMilli      int64 // суммарный спрос после HeadroomPercent
	DemandMemMiB        int64
	RawCPUMilli         int64 // спрос до headroom (для отчёта)
	RawMemMiB           int64
	Pools               []NodePoolAlloc // один или несколько флейворов (смешанный пул)
	CPUUtilization      float64         // % занятости выбранной конфигурации по CPU
	MemUtilization      float64         // % занятости по памяти
	OptimizationPercent float64         // % экономии смешанного пула vs лучший однородный
	costPerHour         float64         // внутренняя условная стоимость выбранного варианта
	baselineCostPerHour float64         // внутренняя условная стоимость лучшего однородного варианта
}

// TotalNodes — суммарное число узлов во всех пулах решения.
func (r EnvSizingResult) TotalNodes() int {
	n := 0
	for _, p := range r.Pools {
		n += p.Count
	}
	return n
}

// PoolSummary — человекочитаемое описание состава пула, напр.
// "2 x s-16x64 + 3 x s-4x16".
func (r EnvSizingResult) PoolSummary() string {
	var sb strings.Builder
	for i, p := range r.Pools {
		if i > 0 {
			sb.WriteString(" + ")
		}
		sb.WriteString(fmt.Sprintf("%d x %s", p.Count, p.Flavor.Name))
	}
	return sb.String()
}
