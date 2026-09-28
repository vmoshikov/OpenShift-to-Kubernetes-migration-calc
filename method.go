package main

import (
	"fmt"
	"math"
)

// Методика расчёта размера кластера по сумме Limits (Таблица 2):
//  1. Базис V — сумма CPU limits всех подов контура в ядрах (после пересчёта
//     реплик и override), спрос по памяти — сумма memory limits в GiB.
//  2. Размер кластера = V × коэффициент Таблицы 2 (зависит от диапазона V
//     и типоразмера ноды 8/16 vCPU).
//  3. Число нод = ceil(размер / ядер ноды), не менее 2. Если номинального
//     RAM на столько нод не хватает под спрос — нод добавляется до покрытия
//     (RAMBound), чтобы любой вариант был рабочим.
//  4. RAM ноды = ядра ноды × 2 или × 4 GiB.
//  5. Перебор {8, 16 vCPU} × {×2, ×4} — 4 варианта, выбор лучшего.

// tableCoefRow — строка Таблицы 2: верхняя граница диапазона V (включительно)
// и коэффициенты корректировки для нод 8 и 16 vCPU.
type tableCoefRow struct {
	MaxV   float64
	Coef8  float64
	Coef16 float64
	Range  string
}

var table2 = []tableCoefRow{
	{16, 2.05, 1.50, "V ≤ 16"},
	{32, 1.61, 1.50, "16 < V ≤ 32"},
	{64, 1.45, 1.36, "32 < V ≤ 64"},
	{256, 1.35, 1.27, "64 < V ≤ 256"},
	{512, 1.34, 1.26, "256 < V ≤ 512"},
	{math.Inf(1), 1.33, 1.25, "V > 512"},
}

var (
	tableNodeCPUs   = []int{8, 16}
	tableRAMFactors = []int{2, 4}
)

const tableMinNodes = 2

// methodologyVersion — версия методики (таблица, перебор, правило выбора);
// попадает в JSON-отчёт. Менять при изменении любого из них.
const methodologyVersion = "1"

// TableVariant — один вариант перебора методики (типоразмер ноды × RAM-коэффициент).
type TableVariant struct {
	NodeCPU       int     // ядер на ноду (8 | 16)
	RAMFactor     int     // RAM ноды = NodeCPU × RAMFactor GiB
	Coef          float64 // коэффициент Таблицы 2
	CoefRange     string  // диапазон V, из которого взят коэффициент
	ClusterCores  float64 // V × Coef
	Nodes         int
	NodeRAMGiB    int
	NominalRAMGiB int     // Nodes × NodeRAMGiB
	RAMBound      bool    // число нод поднято сверх CPU-расчёта, чтобы покрыть RAM
	CPUUtil       float64 // % плотности по CPU: V / (Nodes × NodeCPU)
	RAMUtil       float64 // % плотности по RAM: спрос / номинальный RAM
}

// NominalCores — суммарные ядра нод варианта.
func (v TableVariant) NominalCores() int {
	return v.Nodes * v.NodeCPU
}

// Name — типоразмер ноды, напр. "16 vCPU / 64 GiB".
func (v TableVariant) Name() string {
	return fmt.Sprintf("%d vCPU / %d GiB", v.NodeCPU, v.NodeRAMGiB)
}

// TableMethodResult — результат методики для одного контура.
type TableMethodResult struct {
	UserCores  float64 // V
	UserRAMGiB float64
	Variants   []TableVariant
	Best       TableVariant
}

// tableCoef возвращает коэффициент Таблицы 2 и описание диапазона для V и ноды.
func tableCoef(v float64, nodeCPU int) (float64, string) {
	for _, row := range table2 {
		if v <= row.MaxV {
			if nodeCPU == 8 {
				return row.Coef8, row.Range
			}
			return row.Coef16, row.Range
		}
	}
	return 0, "" // недостижимо: последняя строка — +Inf
}

// CalculateTableMethod считает методику по уже посчитанной сумме limits контура.
func CalculateTableMethod(limitCPUMilli, limitMemMiB int64) TableMethodResult {
	res := TableMethodResult{
		UserCores:  float64(limitCPUMilli) / 1000,
		UserRAMGiB: float64(limitMemMiB) / 1024,
	}
	for _, nodeCPU := range tableNodeCPUs {
		coef, rng := tableCoef(res.UserCores, nodeCPU)
		size := res.UserCores * coef
		nodes := int(math.Ceil(size / float64(nodeCPU)))
		if nodes < tableMinNodes {
			nodes = tableMinNodes
		}
		for _, f := range tableRAMFactors {
			nodeRAM := nodeCPU * f
			n := nodes
			byRAM := int(math.Ceil(res.UserRAMGiB / float64(nodeRAM)))
			if byRAM > n {
				n = byRAM
			}
			v := TableVariant{
				NodeCPU: nodeCPU, RAMFactor: f,
				Coef: coef, CoefRange: rng, ClusterCores: size,
				Nodes: n, NodeRAMGiB: nodeRAM, NominalRAMGiB: n * nodeRAM,
				RAMBound: n > nodes,
			}
			v.CPUUtil = 100 * res.UserCores / float64(v.NominalCores())
			v.RAMUtil = 100 * res.UserRAMGiB / float64(v.NominalRAMGiB)
			res.Variants = append(res.Variants, v)
		}
	}
	res.Best = bestTableVariant(res.Variants)
	return res
}

// bestTableVariant выбирает самый плотный вариант: 1) минимум нод;
// 2) наименьший номинальный RAM (все варианты покрывают спрос по RAM, см.
// RAMBound); 3) при равенстве — выше плотность по CPU.
func bestTableVariant(vs []TableVariant) TableVariant {
	best := vs[0]
	for _, v := range vs[1:] {
		switch {
		case v.Nodes != best.Nodes:
			if v.Nodes < best.Nodes {
				best = v
			}
		case v.NominalRAMGiB != best.NominalRAMGiB:
			if v.NominalRAMGiB < best.NominalRAMGiB {
				best = v
			}
		case v.CPUUtil > best.CPUUtil:
			best = v
		}
	}
	return best
}

// OverrideEffect — эффект override на методику: базис и итог до/после.
type OverrideEffect struct {
	Label                     string
	Level                     string
	CoresBefore, CoresAfter   float64 // V
	RAMBefore, RAMAfter       float64 // спрос RAM, GiB
	SizeBefore, SizeAfter     float64 // размер кластера V × коэффициент, ядер
	NodesBefore, NodesAfter   int
	NomCPUBefore, NomCPUAfter int  // номинальные ядра кластера
	NomRAMBefore, NomRAMAfter int  // номинальный RAM кластера, GiB
	Compressed                int  // сколько limits сжато
	Auto                      bool // уровень подобран RecommendOverride
}

// Worse — override увеличил кластер: V ушло ниже границы диапазона Таблицы 2,
// где коэффициент выше (напр. 33 → 32 ядра: 1.36 → 1.50 для 16 vCPU).
func (e OverrideEffect) Worse() bool {
	return e.SizeAfter > e.SizeBefore || e.NodesAfter > e.NodesBefore || e.NomCPUAfter > e.NomCPUBefore || e.NomRAMAfter > e.NomRAMBefore
}

// Add суммирует эффекты по контурам (для строки «итого»).
func (e *OverrideEffect) Add(o OverrideEffect) {
	e.CoresBefore += o.CoresBefore
	e.CoresAfter += o.CoresAfter
	e.RAMBefore += o.RAMBefore
	e.RAMAfter += o.RAMAfter
	e.SizeBefore += o.SizeBefore
	e.SizeAfter += o.SizeAfter
	e.NodesBefore += o.NodesBefore
	e.NodesAfter += o.NodesAfter
	e.NomCPUBefore += o.NomCPUBefore
	e.NomCPUAfter += o.NomCPUAfter
	e.NomRAMBefore += o.NomRAMBefore
	e.NomRAMAfter += o.NomRAMAfter
	e.Compressed += o.Compressed
}

// Effect — эффект override в контуре (нули в «после» не бывает: без override
// before == after).
func (r EnvSizingResult) Effect() OverrideEffect {
	b, a := r.TableBase, r.Table
	return OverrideEffect{
		Label: r.Env.Label, Level: r.Env.Override.Level,
		CoresBefore: b.UserCores, CoresAfter: a.UserCores,
		RAMBefore: b.UserRAMGiB, RAMAfter: a.UserRAMGiB,
		SizeBefore: b.Best.ClusterCores, SizeAfter: a.Best.ClusterCores,
		NodesBefore: b.Best.Nodes, NodesAfter: a.Best.Nodes,
		NomCPUBefore: b.Best.NominalCores(), NomCPUAfter: a.Best.NominalCores(),
		NomRAMBefore: b.Best.NominalRAMGiB, NomRAMAfter: a.Best.NominalRAMGiB,
		Compressed: r.OverriddenLimits,
		Auto:       r.Recommendation != nil,
	}
}

// fmtChange — «было → стало (−Δ, −%)» в читаемом виде, напр. "59.1 → 52.8 (−6.3, −10.6%)".
func fmtChange(before, after float64, prec int) string {
	s := fmt.Sprintf("%.*f → %.*f", prec, before, prec, after)
	if before == after {
		return s + " (без изменений)"
	}
	d := after - before
	sign := "+"
	if d < 0 {
		sign = "−"
		d = -d
	}
	if before == 0 {
		return fmt.Sprintf("%s (%s%.*f)", s, sign, prec, d)
	}
	return fmt.Sprintf("%s (%s%.*f, %s%.1f%%)", s, sign, prec, d, sign, 100*d/before)
}
