package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	source := flag.String("source", "mock", "источник данных: mock | kubectl | oc | paste | llm-paste")
	namespace := flag.String("namespace", "all", "namespace для kubectl/oc источника ('all' — все неймспейсы)")
	envFilter := flag.String("env", "all", "какие контуры считать: all | dev | ift | psi | prom")
	review := flag.Bool("review", false, "запросить ревью плана у OpenAI-совместимой модели")
	transform := flag.String("transform", "", "конвертация OpenShift-сущностей вместо sizing: routes | dc | scc | all (Workstream 1, требует oc)")
	outDir := flag.String("out", "./transformed", "каталог для сгенерированных манифестов при -transform")
	uiAddr := flag.String("ui", "", "запустить минимальный веб-UI на адресе (напр. 127.0.0.1:8080) вместо CLI-режима")
	skipEnvs := flag.String("skip", "", "пропустить контуры (исключить из расчёта), через запятую: dev,ift,psi,prom")
	from := flag.String("from", "prom", "с какого стенда сняты входные данные: dev | ift | psi | prom (реплики пересчитываются от него)")
	ovr := DefaultLimitsOverride()
	override := flag.String("override", "", "интенсивность сжатия limits к requests: уровень для всех контуров (off | soft | medium | hard | custom | auto) или по контурам: dev:hard,prom:auto; custom берёт -override-cpu-*/-override-mem-*, auto подбирает уровень с лучшим уплотнением")
	flag.Float64Var(&ovr.CPU.Ratio, "override-cpu-ratio", ovr.CPU.Ratio, "override: порог limits/requests по CPU")
	flag.Float64Var(&ovr.CPU.Target, "override-cpu-target", ovr.CPU.Target, "override: новый CPU limits = max(requests, floor) * target")
	flag.Int64Var(&ovr.CPU.Floor, "override-cpu-floor", ovr.CPU.Floor, "override: пол CPU requests, millicores")
	flag.Float64Var(&ovr.Mem.Ratio, "override-mem-ratio", ovr.Mem.Ratio, "override: порог limits/requests по памяти")
	flag.Float64Var(&ovr.Mem.Target, "override-mem-target", ovr.Mem.Target, "override: новый mem limits = max(requests, floor) * target")
	flag.Int64Var(&ovr.Mem.Floor, "override-mem-floor", ovr.Mem.Floor, "override: пол mem requests, MiB")
	appProfile := flag.String("profile", "mixed", "профиль приложения для подбора baremetal: oltp | mixed | dwh")
	serversFile := flag.String("servers", "", "каталог baremetal-серверов (JSON-массив {name, cores, ram_gib, disks}); по умолчанию встроенный")
	pk := DefaultPackingParams()
	flag.IntVar(&pk.MaxPodsPerNode, "max-pods", pk.MaxPodsPerNode, "лимит подов на ноду (kubelet maxPods)")
	flag.IntVar(&pk.MaxPodsPerCluster, "max-cluster-pods", pk.MaxPodsPerCluster, "лимит подов на кластер (etcd/API-сервер)")
	inventoryFile := flag.String("inventory", "", "CSV пула подготовленных серверов (product,node_id,node_ci,node_compute_id,available,code,vendor_title,model_title,params) — выбрать размещение из них")
	placementOut := flag.String("placement-out", "", "записать назначение серверов по контурам в CSV")
	jsonOut := flag.Bool("json", false, "вывести план машиночитаемым JSON в stdout (для агента/оркестратора) вместо текста")
	manifestsDir := flag.String("manifests", "", "сгенерировать манифесты под посчитанные контуры в каталог (replicas + limits после override)")
	flag.Parse()

	profiles := DefaultProfiles()
	fromEnv, err := ProfileByKey(profiles, *from)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка -from:", err)
		os.Exit(1)
	}
	if pk.Profile, err = ProfileByKeyApp(*appProfile); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка -profile:", err)
		os.Exit(1)
	}
	if *serversFile != "" {
		if pk.Servers, err = LoadServers(*serversFile); err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка -servers:", err)
			os.Exit(1)
		}
	}
	levels, err := parseOverrideLevels(*override)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка -override:", err)
		os.Exit(1)
	}

	skip := map[string]bool{}
	for _, k := range strings.Split(*skipEnvs, ",") {
		if k = strings.TrimSpace(k); k != "" {
			skip[k] = true
		}
	}

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

	text := !*jsonOut
	if text {
		fmt.Printf("Источник: %s | Namespace: %s | Данные со стенда: %s\n", *source, *namespace, fromEnv.Label)
		fmt.Printf("Деплойментов: %d\n", len(deployments))
		totalPods := 0
		for _, d := range deployments {
			totalPods += d.Replicas
		}
		fmt.Printf("Подов (на исходном стенде %s): %d\n", fromEnv.Label, totalPods)
		fmt.Println("Базис: limits из манифестов (фактическое потребление из Prometheus не подключено — см. prometheus.go)")
		if ws := AnalyzeDeployments(deployments); len(ws) > 0 {
			fmt.Printf("Предупреждения по данным (%d):\n", len(ws))
			for _, w := range ws {
				fmt.Println("  " + w.String())
			}
		}
		fmt.Println()
	}

	var results []EnvSizingResult
	for _, env := range profiles {
		if (*envFilter != "all" && *envFilter != env.Key) || skip[env.Key] {
			continue
		}
		env.Override = ovr
		rec, err := ApplyOverrideLevel(deployments, fromEnv, &env, levels.forEnv(env.Key))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка -override:", err)
			os.Exit(1)
		}
		res := CalculateEnvSizing(deployments, fromEnv, env)
		res.Recommendation = rec
		packing := PackEnv(res.Deployments, env, pk)
		res.Packing = &packing
		results = append(results, res)
		if text {
			printEnvReport(res)
		}
	}

	rep := BuildPlanReport(*source, *namespace, fromEnv, deployments, results)
	rep.Packing = &pk

	if *inventoryFile != "" {
		f, err := os.Open(*inventoryFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка -inventory:", err)
			os.Exit(1)
		}
		inv, warns, err := ParseInventoryCSV(f)
		f.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка -inventory:", err)
			os.Exit(1)
		}
		pl := PlaceOnInventory(results, inv, warns, pk)
		rep.Placement = &pl
		if text {
			printPlacement(pl)
		}
		if *placementOut != "" {
			out, err := os.Create(*placementOut)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Ошибка -placement-out:", err)
				os.Exit(1)
			}
			if err := WritePlacementCSV(out, pl); err != nil {
				fmt.Fprintln(os.Stderr, "Ошибка -placement-out:", err)
			}
			out.Close()
			if text {
				fmt.Printf("Назначение серверов записано в %s\n", *placementOut)
			}
		}
	}
	if text {
		printTotalEffect(results)
		printBaremetalSummary(results)
	}

	if *manifestsDir != "" {
		m, err := GenerateManifests(*manifestsDir, results)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка генерации манифестов:", err)
			os.Exit(1)
		}
		rep.Manifests = &m
		if text {
			fmt.Printf("\n=== Манифесты: %d файлов в %s ===\n", len(m.Files), m.Dir)
			for _, w := range m.Warnings {
				fmt.Println("  " + w)
			}
		}
	}

	if *review {
		cfg := LoadReviewConfigFromEnv()
		reviewText, err := ReviewSizingPlan(cfg, results)
		switch {
		case err != nil:
			rep.ReviewError = err.Error()
			if text {
				fmt.Fprintln(os.Stderr, "Ревью недоступно:", err)
			}
		case text:
			fmt.Println("\n=== Ревью плана моделью (" + cfg.Model + ") ===")
			fmt.Println(reviewText)
		default:
			rep.Review = reviewText
		}
	}

	if *jsonOut {
		if err := rep.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка вывода JSON:", err)
			os.Exit(1)
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
	if r.NoLimits > 0 {
		fmt.Printf("  Limits не заданы:      %d шт., вместо них взяты requests\n", r.NoLimits)
	}

	t := r.Table
	fmt.Printf("  Методика (сумма Limits × коэффициент Таблицы 2):\n")
	fmt.Printf("    V:                   %.2f ядер CPU, %.1f GiB RAM\n", t.UserCores, t.UserRAMGiB)
	for _, v := range t.Variants {
		mark := " "
		if v == t.Best {
			mark = "*"
		}
		ramNote := ""
		if v.RAMBound {
			ramNote = ", ноды добавлены под RAM"
		}
		fmt.Printf("   %s %-17s коэфф %.2f (%s) → %.1f ядер → %d нод = %d ядер / %d GiB; плотность CPU %.0f%%, RAM %.0f%%%s\n",
			mark, v.Name(), v.Coef, v.CoefRange, v.ClusterCores, v.Nodes, v.NominalCores(), v.NominalRAMGiB,
			v.CPUUtil, v.RAMUtil, ramNote)
	}
	fmt.Printf("    Итог:                %d x %s\n", t.Best.Nodes, t.Best.Name())

	if rec := r.Recommendation; rec != nil {
		fmt.Printf("  override auto → %s: %s\n", rec.Level, rec.Reason)
		for _, c := range rec.Candidates {
			worse := ""
			if c.Worse {
				worse = " ⚠ кластер растёт — исключён"
			}
			fmt.Printf("    %-7s V %.2f ядер → %d нод, %d ядер / %d GiB, сжато %d%s\n",
				c.Level, c.UserCores, c.Nodes, c.NominalCores, c.NominalRAM, c.Compressed, worse)
		}
	}
	if r.Env.Override.Enabled() {
		e := r.Effect()
		fmt.Printf("  ★override (%s), сжато limits: %d\n", e.Level, e.Compressed)
		printEffect("    ", e)
		if e.Worse() {
			fmt.Printf("    ВНИМАНИЕ: override увеличил расчётный кластер — V ушло в диапазон Таблицы 2 с большим коэффициентом; ослабьте уровень\n")
		}
	}

	if r.Packing != nil {
		printPacking(*r.Packing)
	}
	fmt.Println()
}

func printPacking(p PackingResult) {
	s := p.PodStats
	fmt.Printf("  Подбор baremetal (раскладка %d подов):\n", p.Pods)
	fmt.Printf("    Поды (requests): CPU p50 %dm / p95 %dm / max %dm; RAM p50 %dMi / p95 %dMi / max %dMi; %.1f GiB на ядро\n",
		s.CPUP50Milli, s.CPUP95Milli, s.CPUMaxMilli, s.MemP50MiB, s.MemP95MiB, s.MemMaxMiB, s.GiBPerCore)
	fmt.Printf("    Характер: %s\n", s.ProfileHint)
	if p.Verdict.Worthwhile {
		fmt.Printf("    Baremetal: %s\n", p.Verdict.Message)
	} else {
		fmt.Printf("    ⚠ %s\n", p.Verdict.Message)
	}
	for _, o := range p.Options {
		mark := " "
		if p.Best != nil && o.Server.Name == p.Best.Server.Name {
			mark = "*"
		}
		if !o.Feasible {
			fmt.Printf("   %s %-14s не подходит: %s\n", mark, o.Server.Name, o.Reason)
			continue
		}
		spare := ""
		if o.SpareNodes > 0 {
			spare = fmt.Sprintf(" (+%d N+1)", o.SpareNodes)
		}
		fmt.Printf("   %s %-14s %d%s серв. = %d ядер / %d GiB; req CPU %.0f%% RAM %.0f%%; уплотнение CPU ×%.2f RAM ×%.2f; подов/нода %.0f (max %d); узкое место: %s\n",
			mark, o.Server.Name, o.NodesPacked, spare, o.TotalCores, o.TotalRAMGiB, o.CPUReqUtil, o.MemReqUtil,
			o.CPULimitK, o.MemLimitK, o.PodsPerNode, o.MaxPodsOnNode, o.Bottleneck)
	}
	if b := p.Best; b != nil {
		fmt.Printf("    Итог:                %d x %s (%d ядер, %d GiB, %s), уплотнение CPU ×%.2f\n",
			b.Nodes, b.Server.Name, b.Server.Cores, b.Server.RAMGiB, b.Server.Disks, b.CPULimitK)
	}
	for _, w := range p.Warnings {
		fmt.Println("    ВНИМАНИЕ: " + w)
	}
}

func printEffect(indent string, e OverrideEffect) {
	fmt.Printf("%sV, ядер:              %s\n", indent, fmtChange(e.CoresBefore, e.CoresAfter, 2))
	fmt.Printf("%sRAM спрос, GiB:       %s\n", indent, fmtChange(e.RAMBefore, e.RAMAfter, 1))
	fmt.Printf("%sРазмер кластера, ядер: %s\n", indent, fmtChange(e.SizeBefore, e.SizeAfter, 1))
	fmt.Printf("%sНод:                  %s\n", indent, fmtChange(float64(e.NodesBefore), float64(e.NodesAfter), 0))
	fmt.Printf("%sЁмкость, ядер:        %s\n", indent, fmtChange(float64(e.NomCPUBefore), float64(e.NomCPUAfter), 0))
	fmt.Printf("%sЁмкость RAM, GiB:     %s\n", indent, fmtChange(float64(e.NomRAMBefore), float64(e.NomRAMAfter), 0))
}

// printTotalEffect — суммарный эффект override по всем контурам с override.
func printTotalEffect(results []EnvSizingResult) {
	var total OverrideEffect
	any := false
	for _, r := range results {
		if r.Env.Override.Enabled() {
			total.Add(r.Effect())
			any = true
		}
	}
	if !any {
		return
	}
	fmt.Printf("=== Эффект override, итого по контурам с override (сжато limits: %d) ===\n", total.Compressed)
	printEffect("  ", total)
}

// overrideLevels — разобранный -override: общий уровень и/или уровни по контурам.
type overrideLevels struct {
	all   string
	byEnv map[string]string
}

func (l overrideLevels) forEnv(key string) string {
	if lvl, ok := l.byEnv[key]; ok {
		return lvl
	}
	return l.all
}

// parseOverrideLevels разбирает "medium" или "dev:hard,prom:soft" (можно смешивать:
// "soft,prom:hard" — soft везде, кроме ПРОМ).
func parseOverrideLevels(spec string) (overrideLevels, error) {
	l := overrideLevels{all: "off", byEnv: map[string]string{}}
	profiles := DefaultProfiles()
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, lvl, perEnv := strings.Cut(part, ":")
		if !perEnv {
			lvl = part
		}
		if _, ok := overridePresets[lvl]; !ok && lvl != "off" && lvl != "custom" && lvl != "auto" {
			return l, fmt.Errorf("неизвестный уровень override %q (ожидается off | soft | medium | hard | custom | auto)", lvl)
		}
		if !perEnv {
			l.all = part
			continue
		}
		if _, err := ProfileByKey(profiles, key); err != nil {
			return l, err
		}
		l.byEnv[key] = lvl
	}
	return l, nil
}

// printBaremetalSummary — итоговый вердикт по baremetal по всем контурам:
// то, что стоит сказать клиенту первым.
func printBaremetalSummary(results []EnvSizingResult) {
	var no []string
	for _, r := range results {
		if r.Packing != nil && !r.Packing.Verdict.Worthwhile {
			no = append(no, r.Env.Label)
		}
	}
	switch {
	case len(no) == 0:
		fmt.Println("=== Baremetal: все контуры не меньше минимального кластера ===")
	case len(no) == len(results):
		fmt.Println("=== Baremetal нецелесообразен ни для одного контура — рекомендуем виртуальный managed K8s ===")
	default:
		fmt.Printf("=== Baremetal нецелесообразен для: %s — для них виртуальный managed K8s ===\n", strings.Join(no, ", "))
	}
}

func printPlacement(pl PlacementResult) {
	fmt.Printf("\n=== Размещение на пуле серверов (доступно %d) ===\n", pl.PoolTotal)
	for _, e := range pl.Envs {
		fmt.Printf("--- %s ---\n", e.Label)
		if e.Option == nil {
			fmt.Printf("  ⚠ %s\n", e.Shortage)
			continue
		}
		o := e.Option
		kind := "однородный"
		if e.Mixed {
			kind = "смешанный"
		}
		var parts []string
		for _, c := range o.Composition {
			parts = append(parts, fmt.Sprintf("%d × %s", c.Count, c.Server.Name))
		}
		spare := ""
		if o.SpareNodes > 0 {
			spare = fmt.Sprintf(", из них %d N+1", o.SpareNodes)
		}
		fmt.Printf("  %d серв. (%s%s): %s = %d ядер / %d GiB; уплотнение CPU ×%.2f; узкое место: %s\n",
			o.Nodes, kind, spare, strings.Join(parts, " + "), o.TotalCores, o.TotalRAMGiB, o.CPULimitK, o.Bottleneck)
		for _, s := range e.Servers {
			fmt.Printf("    %-12s %-10s %-26s %s %s (%d ядер / %d GiB)\n", s.NodeCI, s.ComputeID, s.Code, s.Vendor, s.Model, s.Cores, s.RAMGiB)
		}
	}
	if len(pl.Remaining) > 0 {
		fmt.Println("  Осталось в пуле:")
		for _, t := range pl.Remaining {
			fmt.Printf("    %d × %s\n", t.Count, t.Type)
		}
	}
	if len(pl.Skipped) > 0 {
		fmt.Printf("  Не в пуле (%d):\n", len(pl.Skipped))
		for _, s := range pl.Skipped {
			fmt.Println("    " + s)
		}
	}
}
