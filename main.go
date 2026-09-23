package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	source := flag.String("source", "mock", "источник данных: mock | kubectl | oc | paste | llm-paste")
	namespace := flag.String("namespace", "all", "namespace для kubectl/oc источника ('all' — все неймспейсы)")
	envFilter := flag.String("env", "all", "какие контуры считать: all | dev | ift | psi | prom")
	review := flag.Bool("review", false, "запросить ревью плана у OpenAI-совместимой модели")
	transform := flag.String("transform", "", "конвертация OpenShift-сущностей вместо sizing: routes | dc | scc | all (Workstream 1, требует oc)")
	outDir := flag.String("out", "./transformed", "каталог для сгенерированных манифестов при -transform")
	uiAddr := flag.String("ui", "", "запустить минимальный веб-UI на адресе (напр. 127.0.0.1:8080) вместо CLI-режима")
	flag.Parse()

	if *uiAddr != "" {
		if err := RunWebUI(*uiAddr); err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка веб-UI:", err)
			os.Exit(1)
		}
		return
	}

	if *transform != "" {
		if err := RunTransform(*transform, *namespace, *outDir); err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка трансформации:", err)
			os.Exit(1)
		}
		return
	}

	deployments, err := loadDeployments(*source, *namespace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка получения данных стенда:", err)
		os.Exit(1)
	}

	fmt.Printf("Источник: %s | Namespace: %s\n", *source, *namespace)
	fmt.Printf("Деплойментов: %d\n", len(deployments))
	totalPods := 0
	for _, d := range deployments {
		totalPods += d.Replicas
	}
	fmt.Printf("Подов (PROM-baseline из исходного стенда): %d\n\n", totalPods)

	flavors := DefaultFlavors()
	var results []EnvSizingResult
	for _, env := range DefaultProfiles() {
		if *envFilter != "all" && *envFilter != env.Key {
			continue
		}
		res := CalculateEnvSizing(deployments, env, flavors)
		results = append(results, res)
		printEnvReport(res)
	}

	printTotalOptimization(results)

	if *review {
		cfg := LoadReviewConfigFromEnv()
		fmt.Println("\n=== Ревью плана моделью (" + cfg.Model + ") ===")
		text, err := ReviewSizingPlan(cfg, results)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Ревью недоступно:", err)
		} else {
			fmt.Println(text)
		}
	}
}

func loadDeployments(source, namespace string) ([]DeploymentSpec, error) {
	switch source {
	case "mock":
		return MockDeployments(), nil
	case "kubectl":
		return FetchDeploymentsFromCluster("kubectl", namespace)
	case "oc":
		return FetchDeploymentsFromCluster("oc", namespace)
	case "paste":
		return ReadDeploymentsFromStdinJSON()
	case "llm-paste":
		return ReadDeploymentsFromLLM(LoadReviewConfigFromEnv())
	default:
		return nil, fmt.Errorf("неизвестный источник %q (ожидается mock | kubectl | oc | paste | llm-paste)", source)
	}
}

func printEnvReport(r EnvSizingResult) {
	fmt.Printf("--- Контур: %s ---\n", r.Env.Label)
	fmt.Printf("  Подов:                 %d\n", r.TotalPods)
	fmt.Printf("  Спрос (без headroom):  %d m CPU, %d MiB RAM\n", r.RawCPUMilli, r.RawMemMiB)
	fmt.Printf("  Спрос (+%.0f%% headroom): %d m CPU, %d MiB RAM\n", r.Env.HeadroomPercent*100, r.DemandCPUMilli, r.DemandMemMiB)
	fmt.Printf("  Рекомендация:          %s (overcommit CPU x%.1f)\n", r.PoolSummary(), r.Env.CPUOvercommit)
	fmt.Printf("  Утилизация узлов:      CPU %.1f%%, RAM %.1f%%\n", r.CPUUtilization, r.MemUtilization)
	fmt.Printf("  Оптимизация пула (смешанный vs однородный): %.1f%%\n\n", r.OptimizationPercent)
}

func printTotalOptimization(results []EnvSizingResult) {
	var costSum, baselineSum float64
	for _, r := range results {
		costSum += r.costPerHour
		baselineSum += r.baselineCostPerHour
	}
	var totalPct float64
	if baselineSum > 0 {
		totalPct = 100 * (baselineSum - costSum) / baselineSum
	}
	fmt.Printf("=== Итого оптимизация по всем выбранным контурам (смешанные пулы vs однородные): %.1f%% ===\n", totalPct)
}
