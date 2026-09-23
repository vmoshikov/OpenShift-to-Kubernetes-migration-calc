package main

import "math"

// CalculateEnvSizing считает спрос на ресурсы для одного контура (DEV/IFT/ПСИ/ПРОМ)
// на основе исходных OpenShift-деплойментов и подбирает наиболее эффективный
// (по количеству узлов, затем по стоимости) флейвор из каталога.
func CalculateEnvSizing(deployments []DeploymentSpec, env EnvProfile, flavors []NodeFlavor) EnvSizingResult {
	var rawCPU, rawMem int64
	var totalPods int

	for _, d := range deployments {
		effectiveReplicas := int(math.Round(float64(d.Replicas) * env.ReplicaFactor))
		if effectiveReplicas < env.MinReplicas {
			effectiveReplicas = env.MinReplicas
		}
		per := d.PerPodRequests()
		rawCPU += per.CPUMilli * int64(effectiveReplicas)
		rawMem += per.MemMiB * int64(effectiveReplicas)
		totalPods += effectiveReplicas
	}

	// Запас на HA/failover/рост поверх суммарного спроса.
	demandCPU := int64(math.Ceil(float64(rawCPU) * (1 + env.HeadroomPercent)))
	demandMem := int64(math.Ceil(float64(rawMem) * (1 + env.HeadroomPercent)))

	best := pickBestFit(demandCPU, demandMem, env, flavors)

	return EnvSizingResult{
		Env:                 env,
		TotalPods:           totalPods,
		RawCPUMilli:         rawCPU,
		RawMemMiB:           rawMem,
		DemandCPUMilli:      demandCPU,
		DemandMemMiB:        demandMem,
		Pools:               best.pools,
		CPUUtilization:      best.cpuUtil,
		MemUtilization:      best.memUtil,
		costPerHour:         best.cost,
		baselineCostPerHour: best.baselineCost,
		OptimizationPercent: best.optimizationPercent(),
	}
}

type mixFit struct {
	pools        []NodePoolAlloc
	cpuUtil      float64
	memUtil      float64
	cost         float64 // внутренняя условная стоимость для сравнения вариантов, наружу не выводится
	baselineCost float64 // стоимость лучшего однородного пула — база для расчёта % оптимизации
}

// optimizationPercent — на сколько % смешанный пул дешевле лучшего однородного
// пула того же флейворного каталога. 0, если однородный вариант и оказался лучшим.
func (f mixFit) optimizationPercent() float64 {
	if f.baselineCost <= 0 || f.baselineCost == math.MaxFloat64 {
		return 0
	}
	pct := 100 * (f.baselineCost - f.cost) / f.baselineCost
	if pct < 0 {
		return 0
	}
	return pct
}

// usableCapacity вычитает системный резерв и применяет CPU overcommit контура,
// возвращая полезную ёмкость одного узла флейвора под спрос подов.
func usableCapacity(f NodeFlavor, env EnvProfile) (cpu, mem int64) {
	cpu = int64(float64(f.CPUMilli-env.SystemReserveCPU) * env.CPUOvercommit)
	mem = f.MemMiB - env.SystemReserveMem
	return
}

// pickBestFit перебирает как однородные пулы (один флейвор), так и смешанные
// пары флейворов (крупный "базовый" + мелкий "добивочный" под остаток спроса),
// и выбирает решение с минимальной итоговой стоимостью в час. Смешанные пулы
// нужны, когда спрос плохо делится на крупные узлы и оставляет много
// неиспользуемой ёмкости (типичный случай для маленьких контуров типа DEV/IFT).
func pickBestFit(demandCPU, demandMem int64, env EnvProfile, flavors []NodeFlavor) mixFit {
	homogeneousBest := mixFit{cost: math.MaxFloat64}
	for _, f := range flavors {
		if fit, ok := homogeneousFit(demandCPU, demandMem, env, f); ok && fit.cost < homogeneousBest.cost {
			homogeneousBest = fit
		}
	}

	best := homogeneousBest
	for _, primary := range flavors {
		for _, secondary := range flavors {
			if primary.Name == secondary.Name {
				continue
			}
			if fit, ok := mixedFit(demandCPU, demandMem, env, primary, secondary); ok && fit.cost < best.cost {
				best = fit
			}
		}
	}

	best.baselineCost = homogeneousBest.cost
	return best
}

// homogeneousFit — sizing на одном флейворе (исходный алгоритм).
func homogeneousFit(demandCPU, demandMem int64, env EnvProfile, f NodeFlavor) (mixFit, bool) {
	usableCPU, usableMem := usableCapacity(f, env)
	if usableCPU <= 0 || usableMem <= 0 {
		return mixFit{}, false // флейвор слишком мал даже под системный резерв
	}

	nodesByCPU := int(math.Ceil(float64(demandCPU) / float64(usableCPU)))
	nodesByMem := int(math.Ceil(float64(demandMem) / float64(usableMem)))
	nodeCount := nodesByCPU
	if nodesByMem > nodeCount {
		nodeCount = nodesByMem
	}
	if nodeCount < 1 {
		nodeCount = 1
	}

	return mixFit{
		pools:   []NodePoolAlloc{{Flavor: f, Count: nodeCount}},
		cpuUtil: 100 * float64(demandCPU) / float64(usableCPU*int64(nodeCount)),
		memUtil: 100 * float64(demandMem) / float64(usableMem*int64(nodeCount)),
		cost:    f.RelativeCost * float64(nodeCount),
	}, true
}

// mixedFit перебирает число узлов primary от 0 до покрытия спроса в одиночку
// и на каждом шаге добивает остаток минимальным числом узлов secondary,
// возвращая самую дешёвую из найденных комбинаций.
func mixedFit(demandCPU, demandMem int64, env EnvProfile, primary, secondary NodeFlavor) (mixFit, bool) {
	pCPU, pMem := usableCapacity(primary, env)
	sCPU, sMem := usableCapacity(secondary, env)
	if pCPU <= 0 || pMem <= 0 || sCPU <= 0 || sMem <= 0 {
		return mixFit{}, false
	}

	maxPrimary := int(math.Ceil(math.Max(
		float64(demandCPU)/float64(pCPU),
		float64(demandMem)/float64(pMem),
	)))

	best := mixFit{cost: math.MaxFloat64}
	found := false

	for pCount := 0; pCount <= maxPrimary; pCount++ {
		remCPU := demandCPU - int64(pCount)*pCPU
		remMem := demandMem - int64(pCount)*pMem
		if remCPU < 0 {
			remCPU = 0
		}
		if remMem < 0 {
			remMem = 0
		}

		sCount := 0
		if remCPU > 0 || remMem > 0 {
			sByCPU := int(math.Ceil(float64(remCPU) / float64(sCPU)))
			sByMem := int(math.Ceil(float64(remMem) / float64(sMem)))
			sCount = sByCPU
			if sByMem > sCount {
				sCount = sByMem
			}
		}
		if pCount == 0 && sCount == 0 {
			continue
		}

		cost := primary.RelativeCost*float64(pCount) + secondary.RelativeCost*float64(sCount)
		if cost < best.cost {
			totalCPU := int64(pCount)*pCPU + int64(sCount)*sCPU
			totalMem := int64(pCount)*pMem + int64(sCount)*sMem

			var pools []NodePoolAlloc
			if pCount > 0 {
				pools = append(pools, NodePoolAlloc{Flavor: primary, Count: pCount})
			}
			if sCount > 0 {
				pools = append(pools, NodePoolAlloc{Flavor: secondary, Count: sCount})
			}

			best = mixFit{
				pools:   pools,
				cpuUtil: 100 * float64(demandCPU) / float64(totalCPU),
				memUtil: 100 * float64(demandMem) / float64(totalMem),
				cost:    cost,
			}
			found = true
		}
	}
	return best, found
}
