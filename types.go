package main

import (
	"fmt"
	"math"
)

// ResourceSpec хранит запрошенные ресурсы в базовых единицах:
// CPU — в миллиядрах (1000 = 1 vCPU), Mem — в MiB.
type ResourceSpec struct {
	CPUMilli int64 `json:"cpu_milli"`
	MemMiB   int64 `json:"mem_mib"`
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
	Kind       string // Deployment | DeploymentConfig | StatefulSet
	Replicas   int
	Containers []ContainerSpec
	// Raw — исходный объект из дампа (paste/kubectl/oc) для генерации
	// манифеста с сохранением image/env/probes/volumes. nil для mock и
	// llm-paste — тогда генерируется скелет.
	Raw map[string]interface{}
}

// EnvProfile описывает, как сущности OpenShift-стенда пересчитываются
// при эмуляции конкретного контура (DEV/IFT/ПСИ/ПРОМ).
type EnvProfile struct {
	Key           string         // dev | ift | psi | prom
	Label         string         // человекочитаемое имя
	ReplicaFactor float64        // доля от prod-реплик (например 0.2 для DEV)
	MinReplicas   int            // минимум реплик на деплоймент в этом контуре
	Skip          bool           // исключить контур из расчёта
	Override      LimitsOverride // сжатие limits к requests в этом контуре
}

// LimitsOverride — сжатие limits к requests: если limits контейнера
// расходятся с requests сильнее порога, limits пересчитываются в
// max(requests, Floor) * Target. Корректирует базис «сумма Limits».
// Интенсивность задаётся уровнем (см. OverrideLevels).
type LimitsOverride struct {
	Level string       // off | soft | medium | hard | custom
	CPU   OverrideRule // Floor в millicores
	Mem   OverrideRule // Floor в MiB
}

// OverrideLevels — уровни интенсивности override. Пресеты задают порог и
// цель (одинаково для CPU и памяти), custom оставляет заданные вручную.
// auto — уровень подбирается RecommendOverride (см. ApplyOverrideLevel).
var OverrideLevels = []string{"off", "soft", "medium", "hard", "custom", "auto"}

var overridePresets = map[string]struct{ ratio, target float64 }{
	"soft":   {3.0, 2.0},
	"medium": {2.0, 1.5},
	"hard":   {1.5, 1.2},
}

// Enabled — override включён (любой уровень, кроме off).
func (o LimitsOverride) Enabled() bool {
	return o.Level != "" && o.Level != "off"
}

// SetLevel выставляет уровень; для пресетов перезаписывает порог и цель.
func (o *LimitsOverride) SetLevel(level string) error {
	switch level {
	case "", "off", "custom":
	default:
		p, ok := overridePresets[level]
		if !ok {
			return fmt.Errorf("неизвестный уровень override %q (ожидается off | soft | medium | hard | custom | auto)", level)
		}
		o.CPU.Ratio, o.CPU.Target = p.ratio, p.target
		o.Mem.Ratio, o.Mem.Target = p.ratio, p.target
	}
	o.Level = level
	if !o.Enabled() {
		return nil
	}
	if err := o.CPU.Validate(); err != nil {
		return fmt.Errorf("CPU: %w", err)
	}
	if err := o.Mem.Validate(); err != nil {
		return fmt.Errorf("память: %w", err)
	}
	return nil
}

// OverrideRule — параметры override для одного ресурса (CPU или память).
type OverrideRule struct {
	Ratio  float64 `json:"ratio"`  // порог limits/requests, выше которого limits сжимаются
	Target float64 `json:"target"` // новый limits = max(requests, Floor) * Target
	Floor  int64   `json:"floor"`  // «пол» requests перед умножением на Target
}

// DefaultLimitsOverride — дефолты override (выключен): порог x2.0,
// цель x1.5 (как medium), пол 250m CPU / 256Mi памяти.
func DefaultLimitsOverride() LimitsOverride {
	return LimitsOverride{
		Level: "off",
		CPU:   OverrideRule{Ratio: 2.0, Target: 1.5, Floor: 250},
		Mem:   OverrideRule{Ratio: 2.0, Target: 1.5, Floor: 256},
	}
}

// Validate проверяет, что параметры имеют смысл: порог и цель ≥ 1 (иначе
// limits окажется ниже requests), floor ≥ 0, всё конечно.
func (o OverrideRule) Validate() error {
	if math.IsNaN(o.Ratio) || math.IsInf(o.Ratio, 0) || o.Ratio < 1 {
		return fmt.Errorf("порог должен быть конечным числом ≥ 1, получено %v", o.Ratio)
	}
	if math.IsNaN(o.Target) || math.IsInf(o.Target, 0) || o.Target < 1 {
		return fmt.Errorf("цель должна быть конечным числом ≥ 1, получено %v", o.Target)
	}
	if o.Floor < 0 {
		return fmt.Errorf("floor должен быть ≥ 0, получено %d", o.Floor)
	}
	return nil
}

// apply возвращает limits после override и признак, что он сработал.
// Сжимает только: если пересчитанный limits не меньше исходного, или limits
// не задан, остаётся исходное значение.
func (o OverrideRule) apply(req, lim int64) (int64, bool) {
	if lim <= 0 {
		return lim, false
	}
	if req > 0 && float64(lim) <= float64(req)*o.Ratio {
		return lim, false
	}
	base := req
	if base < o.Floor {
		base = o.Floor
	}
	newLim := int64(math.Ceil(float64(base) * o.Target))
	// Не раздуваем limit, не обнуляем его (0 = «без лимита») и не опускаем
	// ниже requests (K8s отвергнет такой манифест).
	if newLim >= lim || newLim <= 0 || newLim < req {
		return lim, false
	}
	return newLim, true
}

// EnvSizingResult — результат расчёта для одного контура.
type EnvSizingResult struct {
	Env               EnvProfile
	TotalPods         int
	LimitCPUMilli     int64 // сумма limits по подам контура (после override, если включён)
	LimitMemMiB       int64
	OrigLimitCPUMilli int64 // сумма limits до override
	OrigLimitMemMiB   int64
	Deployments       []DeploymentPlan        // поштучный план контура (для манифестов и JSON)
	Recommendation    *OverrideRecommendation // заполнено, если уровень override = auto
	NoLimits          int                     // сколько limits в шаблонах не задано — вместо них взяты requests
	Table             TableMethodResult       // методика: V = сумма Limits × коэффициент Таблицы 2
	TableBase         TableMethodResult       // та же методика без override — для оценки его эффекта
	OverriddenLimits  int                     // сколько limits (CPU/память контейнеров шаблонов) сжато override
}
