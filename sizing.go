package main

// CalculateEnvSizing считает контур (DEV/IFT/ПСИ/ПРОМ) по методике: строит
// поштучный план деплойментов (реплики пересчитаны от стенда-источника,
// override применён), суммирует limits в базис V и подбирает конфигурацию
// нод по Таблице 2. Та же методика на limits без override — TableBase, для
// оценки эффекта override.
func CalculateEnvSizing(deployments []DeploymentSpec, source, env EnvProfile) EnvSizingResult {
	var limCPU, limMem, origLimCPU, origLimMem int64
	var totalPods, overridden, noLimits int
	plans := make([]DeploymentPlan, 0, len(deployments))

	for i := range deployments {
		p := PlanDeployment(&deployments[i], source, env)
		plans = append(plans, p)
		reps := int64(p.Replicas)
		totalPods += p.Replicas
		for _, c := range p.Containers {
			limCPU += c.Limits.CPUMilli * reps
			limMem += c.Limits.MemMiB * reps
			// «До override» — те же limits с подстановкой requests там, где limit не задан.
			origCPU, origMem := c.OrigLimits.CPUMilli, c.OrigLimits.MemMiB
			if c.CPUNoLimit {
				origCPU = c.Requests.CPUMilli
				noLimits++
			}
			if c.MemNoLimit {
				origMem = c.Requests.MemMiB
				noLimits++
			}
			origLimCPU += origCPU * reps
			origLimMem += origMem * reps
			if c.CPUOverridden {
				overridden++
			}
			if c.MemOverridden {
				overridden++
			}
		}
	}

	return EnvSizingResult{
		Env:               env,
		TotalPods:         totalPods,
		LimitCPUMilli:     limCPU,
		LimitMemMiB:       limMem,
		OrigLimitCPUMilli: origLimCPU,
		OrigLimitMemMiB:   origLimMem,
		OverriddenLimits:  overridden,
		Deployments:       plans,
		NoLimits:          noLimits,
		Table:             CalculateTableMethod(limCPU, limMem),
		TableBase:         CalculateTableMethod(origLimCPU, origLimMem),
	}
}
