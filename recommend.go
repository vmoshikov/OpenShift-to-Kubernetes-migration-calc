package main

import "fmt"

// RecommendOverride перебирает уровни override для контура и выбирает тот,
// что даёт лучшее уплотнение по методике, с минимальным риском:
//  1. отбрасываются уровни, при которых расчётный кластер растёт (Worse —
//     немонотонность Таблицы 2 на границах диапазонов);
//  2. минимум нод → минимум ядер ёмкости → минимум RAM ёмкости;
//  3. при равенстве — более мягкий уровень: ёмкость та же, а limits
//     сжаты меньше, значит меньше риск троттлинга CPU и OOMKill.
//
// Порог/цель берутся из пресетов, floor — из текущих настроек контура.
//
// TODO(prometheus): без фактического потребления рекомендация опирается
// только на соотношение limits/requests. С метриками добавить проверку
// «сжатый limit ≥ p99 потребления × запас» и отбрасывать уровни, которые
// её нарушают хотя бы для одного контейнера (+ учитывать CPU throttling
// и историю OOMKilled).
func RecommendOverride(deployments []DeploymentSpec, source, env EnvProfile) OverrideRecommendation {
	rec := OverrideRecommendation{Env: env.Key}
	for _, level := range []string{"off", "soft", "medium", "hard"} {
		e := env
		if err := e.Override.SetLevel(level); err != nil {
			continue // floor из настроек контура невалиден — пресет не применить
		}
		r := CalculateEnvSizing(deployments, source, e)
		eff := r.Effect()
		rec.Candidates = append(rec.Candidates, OverrideCandidate{
			Level:        level,
			Nodes:        r.Table.Best.Nodes,
			NominalCores: r.Table.Best.NominalCores(),
			NominalRAM:   r.Table.Best.NominalRAMGiB,
			ClusterCores: r.Table.Best.ClusterCores,
			UserCores:    r.Table.UserCores,
			UserRAMGiB:   r.Table.UserRAMGiB,
			Compressed:   r.OverriddenLimits,
			Worse:        eff.Worse(),
		})
	}

	best := 0 // off: кластер при нём не растёт по определению
	for i, c := range rec.Candidates {
		if !c.Worse && c.betterThan(rec.Candidates[best]) {
			best = i
		}
	}
	b := rec.Candidates[best]
	rec.Level = b.Level
	base := rec.Candidates[0] // off
	if b.Level == "off" {
		rec.Reason = "override не уменьшает ёмкость кластера — limits не трогаем"
	} else {
		rec.Reason = fmt.Sprintf("самый мягкий уровень с наименьшей ёмкостью: нод %d → %d, ядер %d → %d, RAM %d → %d GiB",
			base.Nodes, b.Nodes, base.NominalCores, b.NominalCores, base.NominalRAM, b.NominalRAM)
	}
	return rec
}

// OverrideRecommendation — результат перебора уровней для контура.
type OverrideRecommendation struct {
	Env        string              `json:"env"`
	Level      string              `json:"level"`
	Reason     string              `json:"reason"`
	Candidates []OverrideCandidate `json:"candidates"`
}

// OverrideCandidate — итог методики при одном уровне override.
type OverrideCandidate struct {
	Level        string  `json:"level"`
	Nodes        int     `json:"nodes"`
	NominalCores int     `json:"nominal_cores"`
	NominalRAM   int     `json:"nominal_ram_gib"`
	ClusterCores float64 `json:"cluster_cores"`
	UserCores    float64 `json:"v_cores"`
	UserRAMGiB   float64 `json:"ram_demand_gib"`
	Compressed   int     `json:"compressed_limits"`
	Worse        bool    `json:"cluster_grows"`
}

// betterThan: меньше нод → меньше ядер → меньше RAM. Кандидаты идут от
// мягкого к жёсткому, поэтому при равенстве остаётся более мягкий.
func (c OverrideCandidate) betterThan(o OverrideCandidate) bool {
	if c.Nodes != o.Nodes {
		return c.Nodes < o.Nodes
	}
	if c.NominalCores != o.NominalCores {
		return c.NominalCores < o.NominalCores
	}
	return c.NominalRAM < o.NominalRAM
}

// ApplyOverrideLevel выставляет контуру уровень override; для "auto" сначала
// подбирает уровень через RecommendOverride и возвращает рекомендацию.
func ApplyOverrideLevel(deployments []DeploymentSpec, source EnvProfile, env *EnvProfile, level string) (*OverrideRecommendation, error) {
	if level != "auto" {
		return nil, env.Override.SetLevel(level)
	}
	rec := RecommendOverride(deployments, source, *env)
	return &rec, env.Override.SetLevel(rec.Level)
}
