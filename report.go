package main

import (
	"encoding/json"
	"io"
	"math"
)

// Машиночитаемый отчёт (-json) — контракт для агента/оркестратора:
// всё, что CLI печатает человеку, плюс поштучный план по деплойментам,
// рекомендации override и предупреждения по данным. Единицы — в именах полей.

// PlanReport — корень JSON-отчёта.
type PlanReport struct {
	SchemaVersion string          `json:"schema_version"`
	Methodology   MethodologyInfo `json:"methodology"`
	Source        string          `json:"source"`
	Namespace     string          `json:"namespace"`
	FromEnv       string          `json:"from_env"`
	Deployments   int             `json:"deployments"`
	SourcePods    int             `json:"source_pods"`
	Basis         BasisInfo       `json:"basis"`
	Envs          []EnvReport     `json:"envs"`
	OverrideSum   *EffectReport   `json:"override_total,omitempty"`
	DataWarnings  []DataWarning   `json:"data_warnings"`
	Manifests     *ManifestResult `json:"manifests,omitempty"`
	Review        string          `json:"review,omitempty"`
	ReviewError   string          `json:"review_error,omitempty"`
}

// reportSchemaVersion — версия JSON-контракта; менять при несовместимых
// изменениях полей, чтобы агент/оркестратор мог это проверить.
const reportSchemaVersion = "1"

// MethodologyInfo — параметры методики, по которой посчитан план: чтобы
// через полгода было видно, по какой таблице считали.
type MethodologyInfo struct {
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	Table      []CoefRowInfo `json:"coef_table"`
	NodeCPUs   []int         `json:"node_cpus"`
	RAMFactors []int         `json:"ram_factors"`
	MinNodes   int           `json:"min_nodes"`
	Selection  string        `json:"selection"`
	Profiles   []ProfileInfo `json:"env_profiles"`
}

// CoefRowInfo — строка Таблицы 2.
type CoefRowInfo struct {
	Range  string  `json:"range"`
	Coef8  float64 `json:"coef_8vcpu"`
	Coef16 float64 `json:"coef_16vcpu"`
}

// ProfileInfo — коэффициенты пересчёта реплик контура.
type ProfileInfo struct {
	Env           string  `json:"env"`
	ReplicaFactor float64 `json:"replica_factor"`
	MinReplicas   int     `json:"min_replicas"`
}

func methodologyInfo() MethodologyInfo {
	m := MethodologyInfo{
		Name: "limits-table2", Version: methodologyVersion,
		NodeCPUs: tableNodeCPUs, RAMFactors: tableRAMFactors, MinNodes: tableMinNodes,
		Selection: "min nodes → min nominal RAM → max CPU density; nodes added to cover RAM",
	}
	for _, r := range table2 {
		m.Table = append(m.Table, CoefRowInfo{Range: r.Range, Coef8: r.Coef8, Coef16: r.Coef16})
	}
	for _, p := range DefaultProfiles() {
		m.Profiles = append(m.Profiles, ProfileInfo{Env: p.Key, ReplicaFactor: p.ReplicaFactor, MinReplicas: p.MinReplicas})
	}
	return m
}

// BasisInfo — на чём основан расчёт; агент должен учитывать это при
// интерпретации (limits ≠ фактическое потребление).
type BasisInfo struct {
	Metric       string   `json:"metric"`        // "limits"
	UsageMetrics string   `json:"usage_metrics"` // "not_connected" | "prometheus"
	Todo         []string `json:"todo,omitempty"`
}

// prometheusTodo — что даст подключение Prometheus (см. prometheus.go).
var prometheusTodo = []string{
	"базис V: p95 CPU / p99 memory working set вместо суммы limits (или как проверка limits сверху)",
	"override: сжатый limit не ниже p99 потребления × запас; учитывать CPU throttling и OOMKilled",
	"реплики: для HPA — max_over_time числа реплик за окно, а не spec.replicas на момент дампа",
	"BestEffort-контейнеры (без requests/limits): базис из фактического потребления",
}

// EnvReport — результат по контуру.
type EnvReport struct {
	Env            string                  `json:"env"`
	Label          string                  `json:"label"`
	Pods           int                     `json:"pods"`
	OverrideLevel  string                  `json:"override_level"`
	OverrideRules  *OverrideRulesInfo      `json:"override_rules,omitempty"`
	Recommendation *OverrideRecommendation `json:"override_recommendation,omitempty"`
	Method         MethodReport            `json:"method"`
	Effect         *EffectReport           `json:"override_effect,omitempty"`
	NoLimits       int                     `json:"limits_missing"`
	Deployments    []DeploymentPlan        `json:"deployment_plans"`
}

// OverrideRulesInfo — фактически применённые порог/цель/floor.
type OverrideRulesInfo struct {
	CPU OverrideRule `json:"cpu"` // floor — millicores
	Mem OverrideRule `json:"mem"` // floor — MiB
}

// MethodReport — методика (сумма Limits × коэффициент Таблицы 2).
type MethodReport struct {
	VCores     float64         `json:"v_cores"`
	RAMGiB     float64         `json:"ram_demand_gib"`
	Best       VariantReport   `json:"best"`
	Variants   []VariantReport `json:"variants"`
	BaseNoOvrd VariantReport   `json:"best_without_override"`
}

// VariantReport — вариант перебора методики.
type VariantReport struct {
	Node          string  `json:"node"`
	NodeCPU       int     `json:"node_cpu"`
	NodeRAMGiB    int     `json:"node_ram_gib"`
	Coef          float64 `json:"coef"`
	CoefRange     string  `json:"coef_range"`
	ClusterCores  float64 `json:"cluster_cores"`
	Nodes         int     `json:"nodes"`
	NominalCores  int     `json:"nominal_cores"`
	NominalRAMGiB int     `json:"nominal_ram_gib"`
	RAMBound      bool    `json:"ram_bound"`
	CPUUtilPct    float64 `json:"cpu_density_pct"`
	RAMUtilPct    float64 `json:"ram_density_pct"`
}

// EffectReport — эффект override: было/стало и % изменения.
type EffectReport struct {
	Compressed   int      `json:"compressed_limits"`
	VCores       Change   `json:"v_cores"`
	RAMGiB       Change   `json:"ram_demand_gib"`
	ClusterCores Change   `json:"cluster_cores"`
	Nodes        Change   `json:"nodes"`
	NominalCores Change   `json:"nominal_cores"`
	NominalRAM   Change   `json:"nominal_ram_gib"`
	ClusterGrows bool     `json:"cluster_grows"`
	Warnings     []string `json:"warnings,omitempty"`
}

// Change — было/стало; Pct < 0 — уменьшение.
type Change struct {
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Pct    float64 `json:"pct"`
}

func change(b, a float64) Change {
	c := Change{Before: b, After: a}
	if b != 0 {
		c.Pct = math.Round(1000*(a-b)/b) / 10 // 1 знак после запятой
	}
	return c
}

func variantReport(v TableVariant) VariantReport {
	return VariantReport{
		Node: v.Name(), NodeCPU: v.NodeCPU, NodeRAMGiB: v.NodeRAMGiB,
		Coef: v.Coef, CoefRange: v.CoefRange, ClusterCores: v.ClusterCores,
		Nodes: v.Nodes, NominalCores: v.NominalCores(), NominalRAMGiB: v.NominalRAMGiB,
		RAMBound: v.RAMBound, CPUUtilPct: v.CPUUtil, RAMUtilPct: v.RAMUtil,
	}
}

func effectReport(e OverrideEffect) *EffectReport {
	r := &EffectReport{
		Compressed:   e.Compressed,
		VCores:       change(e.CoresBefore, e.CoresAfter),
		RAMGiB:       change(e.RAMBefore, e.RAMAfter),
		ClusterCores: change(e.SizeBefore, e.SizeAfter),
		Nodes:        change(float64(e.NodesBefore), float64(e.NodesAfter)),
		NominalCores: change(float64(e.NomCPUBefore), float64(e.NomCPUAfter)),
		NominalRAM:   change(float64(e.NomRAMBefore), float64(e.NomRAMAfter)),
		ClusterGrows: e.Worse(),
	}
	if e.Worse() {
		r.Warnings = append(r.Warnings, "override увеличил расчётный кластер: V ушло в диапазон Таблицы 2 с большим коэффициентом")
	}
	return r
}

// BuildPlanReport собирает JSON-отчёт из результатов расчёта.
func BuildPlanReport(source, namespace string, from EnvProfile, ds []DeploymentSpec, results []EnvSizingResult) PlanReport {
	rep := PlanReport{
		SchemaVersion: reportSchemaVersion,
		Methodology:   methodologyInfo(),
		Source:        source, Namespace: namespace, FromEnv: from.Key,
		Deployments: len(ds),
		Basis: BasisInfo{
			Metric:       "limits",
			UsageMetrics: "not_connected",
			Todo:         prometheusTodo,
		},
		DataWarnings: AnalyzeDeployments(ds),
	}
	if rep.DataWarnings == nil {
		rep.DataWarnings = []DataWarning{}
	}
	for _, d := range ds {
		rep.SourcePods += d.Replicas
	}

	var total OverrideEffect
	anyOverride := false
	for _, r := range results {
		er := EnvReport{
			Env: r.Env.Key, Label: r.Env.Label, Pods: r.TotalPods,
			OverrideLevel:  r.Env.Override.Level,
			Recommendation: r.Recommendation,
			Method: MethodReport{
				VCores: r.Table.UserCores, RAMGiB: r.Table.UserRAMGiB,
				Best:       variantReport(r.Table.Best),
				BaseNoOvrd: variantReport(r.TableBase.Best),
			},
			NoLimits:    r.NoLimits,
			Deployments: r.Deployments,
		}
		if er.OverrideLevel == "" {
			er.OverrideLevel = "off"
		}
		if r.Env.Override.Enabled() {
			er.OverrideRules = &OverrideRulesInfo{CPU: r.Env.Override.CPU, Mem: r.Env.Override.Mem}
		}
		for _, v := range r.Table.Variants {
			er.Method.Variants = append(er.Method.Variants, variantReport(v))
		}
		if r.Env.Override.Enabled() {
			e := r.Effect()
			er.Effect = effectReport(e)
			total.Add(e)
			anyOverride = true
		}
		rep.Envs = append(rep.Envs, er)
	}
	if anyOverride {
		rep.OverrideSum = effectReport(total)
	}
	return rep
}

// WriteJSON пишет отчёт с отступами.
func (r PlanReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}
