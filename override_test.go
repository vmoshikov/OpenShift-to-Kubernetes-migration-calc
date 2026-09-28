package main

import (
	"math"
	"testing"
)

func TestOverrideRuleApply(t *testing.T) {
	medium := OverrideRule{Ratio: 2.0, Target: 1.5, Floor: 250}
	tests := []struct {
		name     string
		rule     OverrideRule
		req, lim int64
		want     int64
		applied  bool
	}{
		{"limit не задан", medium, 500, 0, 0, false},
		{"ratio ровно на пороге — не сжимаем", medium, 500, 1000, 1000, false},
		{"ratio выше порога", medium, 500, 1001, 750, true},
		{"requests > limits (битые данные)", medium, 1000, 500, 500, false},
		{"requests ниже floor: база = floor", medium, 100, 1000, 375, true},
		{"floor×target ≥ limit — не раздуваем", medium, 100, 300, 300, false},
		{"requests = 0, limit задан: база = floor", medium, 0, 1000, 375, true},
		{"requests = 0 и floor = 0: limit не обнуляем", OverrideRule{Ratio: 2, Target: 1.5, Floor: 0}, 0, 1000, 1000, false},
		{"округление вверх", OverrideRule{Ratio: 2, Target: 1.5, Floor: 0}, 333, 1000, 500, true},
		{"target < 1: limit не ниже requests", OverrideRule{Ratio: 2, Target: 0.5, Floor: 0}, 400, 1000, 1000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.rule.apply(tt.req, tt.lim)
			if got != tt.want || ok != tt.applied {
				t.Errorf("apply(%d, %d) = (%d, %v), want (%d, %v)", tt.req, tt.lim, got, ok, tt.want, tt.applied)
			}
		})
	}
}

func TestOverrideRuleValidate(t *testing.T) {
	bad := []OverrideRule{
		{Ratio: 0.5, Target: 1.5, Floor: 0},
		{Ratio: 2, Target: 0.9, Floor: 0},
		{Ratio: 2, Target: 1.5, Floor: -1},
		{Ratio: math.NaN(), Target: 1.5},
		{Ratio: 2, Target: math.Inf(1)},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", r)
		}
	}
	if err := DefaultLimitsOverride().CPU.Validate(); err != nil {
		t.Errorf("default rule invalid: %v", err)
	}
}

func deploy(name string, replicas int, cs ...ContainerSpec) DeploymentSpec {
	return DeploymentSpec{Name: name, Namespace: "t", Replicas: replicas, Containers: cs}
}

func ctr(reqCPU, reqMem, limCPU, limMem int64) ContainerSpec {
	return ContainerSpec{
		Name:     "c",
		Requests: ResourceSpec{CPUMilli: reqCPU, MemMiB: reqMem},
		Limits:   ResourceSpec{CPUMilli: limCPU, MemMiB: limMem},
	}
}

func promWith(level string) EnvProfile {
	p, _ := ProfileByKey(DefaultProfiles(), "prom")
	p.Override.SetLevel(level)
	return p
}

func TestOverrideInSizing(t *testing.T) {
	prom := promWith("medium")

	t.Run("многоконтейнерный под: сжимается только расходящийся контейнер", func(t *testing.T) {
		d := deploy("x", 2,
			ctr(500, 512, 2000, 1024), // CPU x4 → 750m; mem x2 — на пороге
			ctr(500, 512, 600, 600),   // в пределах порога
		)
		r := CalculateEnvSizing([]DeploymentSpec{d}, prom, prom)
		if r.LimitCPUMilli != (750+600)*2 || r.OverriddenLimits != 1 {
			t.Errorf("limits CPU = %d, overridden = %d", r.LimitCPUMilli, r.OverriddenLimits)
		}
		if r.OrigLimitCPUMilli != (2000+600)*2 {
			t.Errorf("orig limits CPU = %d", r.OrigLimitCPUMilli)
		}
	})

	t.Run("limit не задан: в базис идёт requests, override не трогает", func(t *testing.T) {
		d := deploy("x", 1, ctr(300, 256, 0, 0))
		r := CalculateEnvSizing([]DeploymentSpec{d}, prom, prom)
		if r.LimitCPUMilli != 300*2 || r.NoLimits != 2 || r.OverriddenLimits != 0 {
			t.Errorf("limits = %d, noLimits = %d, overridden = %d", r.LimitCPUMilli, r.NoLimits, r.OverriddenLimits)
		}
	})

	t.Run("off: limits не меняются", func(t *testing.T) {
		d := deploy("x", 1, ctr(100, 128, 4000, 4096))
		off := promWith("off")
		r := CalculateEnvSizing([]DeploymentSpec{d}, off, off)
		if r.LimitCPUMilli != r.OrigLimitCPUMilli || r.Table.Best != r.TableBase.Best {
			t.Errorf("off changed limits: %d vs %d", r.LimitCPUMilli, r.OrigLimitCPUMilli)
		}
	})

	t.Run("пустой стенд", func(t *testing.T) {
		r := CalculateEnvSizing(nil, prom, prom)
		if r.Table.Best.Nodes != tableMinNodes {
			t.Errorf("nodes = %d", r.Table.Best.Nodes)
		}
	})
}

// Таблица 2 немонотонна на границах диапазонов: V чуть выше границы даёт
// меньший коэффициент. Override, уменьшив V через границу, может увеличить
// кластер — это надо показать пользователю, а не молча выдать «+» нод.
func TestOverrideCrossingCoefBoundary(t *testing.T) {
	before := CalculateTableMethod(33_000, 0) // 33 ядра: 16 vCPU × 1.36 → 44.9 → 3 ноды
	after := CalculateTableMethod(32_000, 0)  // 32 ядра: 16 vCPU × 1.50 → 48.0 → 3 ноды
	if after.Best.ClusterCores <= before.Best.ClusterCores {
		t.Fatalf("ожидали рост размера кластера на границе: %.1f → %.1f", before.Best.ClusterCores, after.Best.ClusterCores)
	}
	before = CalculateTableMethod(49_000, 0) // 49 × 1.36 = 66.6 → 5 нод
	after = CalculateTableMethod(32_000, 0)  // 32 × 1.50 = 48.0 → 3 ноды
	if after.Best.Nodes > before.Best.Nodes {
		t.Errorf("сжатие далеко за границу не должно давать рост нод")
	}
}

func TestTableRAMBound(t *testing.T) {
	// 4 ядра CPU, но 200 GiB RAM: CPU-расчёт даёт 2 ноды, RAM их не покрывает.
	r := CalculateTableMethod(4_000, 200*1024)
	for _, v := range r.Variants {
		if float64(v.NominalRAMGiB) < r.UserRAMGiB {
			t.Errorf("%s: RAM %d < спрос %.0f", v.Name(), v.NominalRAMGiB, r.UserRAMGiB)
		}
	}
	// 16×64: 4 ноды = 256 GiB; 8×32: 7 нод = 224 GiB — меньше нод побеждает.
	if r.Best.NodeCPU != 16 || r.Best.RAMFactor != 4 || r.Best.Nodes != 4 || !r.Best.RAMBound {
		t.Errorf("best = %+v", r.Best)
	}
}

func TestOverrideEffectWorse(t *testing.T) {
	// 30 ядер без расхождений + 4 пода 250m/800m (x3.2 → 375m): V 33.2 → 31.5,
	// коэффициент 16 vCPU 1.36 → 1.50: размер кластера растёт.
	ds := []DeploymentSpec{
		deploy("big", 30, ctr(1000, 1024, 1000, 1024)),
		deploy("spiky", 4, ctr(250, 256, 800, 256)),
	}
	prom := promWith("medium")
	e := CalculateEnvSizing(ds, prom, prom).Effect()
	if !e.Worse() || e.SizeAfter <= e.SizeBefore {
		t.Errorf("ожидали Worse: size %.1f → %.1f", e.SizeBefore, e.SizeAfter)
	}
	if e.NodesAfter > e.NodesBefore {
		t.Errorf("нод стало больше: %d → %d", e.NodesBefore, e.NodesAfter)
	}
}

func TestSetLevelValidates(t *testing.T) {
	o := DefaultLimitsOverride()
	o.CPU.Target = 0.5
	if err := o.SetLevel("custom"); err == nil {
		t.Error("custom с target 0.5 должен отклоняться")
	}
	if err := o.SetLevel("hard"); err != nil {
		t.Errorf("пресет перезаписывает target, ошибки быть не должно: %v", err)
	}
	o.CPU.Floor = -1
	if err := o.SetLevel("off"); err != nil {
		t.Errorf("off не валидирует параметры: %v", err)
	}
}

func TestRecommendOverride(t *testing.T) {
	prom := promWith("off")

	t.Run("сжатие не снижает ёмкость — остаёмся на off", func(t *testing.T) {
		// DEV mock: hard уменьшает V, но нод/ядер/RAM столько же.
		dev, _ := ProfileByKey(DefaultProfiles(), "dev")
		rec := RecommendOverride(MockDeployments(), prom, dev)
		if rec.Level != "off" {
			t.Errorf("level = %s, %s", rec.Level, rec.Reason)
		}
	})

	t.Run("выбирается самый мягкий из равных по ёмкости", func(t *testing.T) {
		// 20 подов 250m/2 CPU, 512Mi/2Gi — сжимают все уровни:
		// off 4 ноды; soft 2 × 16 vCPU; medium и hard — по 2 × 8 vCPU / 16 GiB.
		// medium и hard равны по ёмкости → берётся более мягкий medium.
		d := deploy("x", 20, ctr(250, 512, 2000, 2048))
		rec := RecommendOverride([]DeploymentSpec{d}, prom, prom)
		if rec.Level != "medium" {
			t.Errorf("level = %s, candidates = %+v", rec.Level, rec.Candidates)
		}
	})

	t.Run("уровень, при котором кластер растёт, исключён", func(t *testing.T) {
		ds := []DeploymentSpec{
			deploy("big", 30, ctr(1000, 1024, 1000, 1024)),
			deploy("spiky", 4, ctr(250, 256, 800, 256)),
		}
		rec := RecommendOverride(ds, prom, prom)
		for _, c := range rec.Candidates {
			if c.Worse && c.Level == rec.Level {
				t.Errorf("выбран уровень %s с ростом кластера", c.Level)
			}
		}
	})

	t.Run("auto через ApplyOverrideLevel", func(t *testing.T) {
		env := prom
		rec, err := ApplyOverrideLevel(MockDeployments(), prom, &env, "auto")
		if err != nil || rec == nil || env.Override.Level != rec.Level {
			t.Errorf("rec = %+v, level = %s, err = %v", rec, env.Override.Level, err)
		}
	})
}

func TestParseK8sDefaults(t *testing.T) {
	ds, err := parseDeploymentListJSON([]byte(`{"items":[
	 {"kind":"StatefulSet","metadata":{"name":"db","namespace":"n"},"spec":{"replicas":0,"template":{"spec":{"containers":[
	   {"name":"db","resources":{"limits":{"cpu":"2","memory":"4Gi"}}}]}}}},
	 {"kind":"Deployment","metadata":{"name":"api","namespace":"n"},"spec":{"template":{"spec":{"containers":[
	   {"name":"api","resources":{"requests":{"cpu":"100m"},"limits":{"cpu":"1"}}}]}}}},
	 {"kind":"Service","metadata":{"name":"svc","namespace":"n"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("ожидали 2 workload (Service отфильтрован), got %d", len(ds))
	}
	db := ds[0].Containers[0]
	if db.Requests.CPUMilli != 2000 || db.Requests.MemMiB != 4096 {
		t.Errorf("request не выставлен = limit: %+v", db.Requests)
	}
	if ds[0].Replicas != 0 || ds[0].Kind != "StatefulSet" || ds[0].Raw == nil {
		t.Errorf("db = replicas %d kind %s raw %v", ds[0].Replicas, ds[0].Kind, ds[0].Raw != nil)
	}
	if ds[1].Replicas != 1 {
		t.Errorf("replicas не задан → 1, got %d", ds[1].Replicas)
	}
	// Override не должен сжимать limit, у которого request был подставлен = limit.
	env := promWith("hard")
	r := CalculateEnvSizing(ds[:1], env, env)
	if r.OverriddenLimits != 0 {
		t.Errorf("сжат limit с неявным request: %d", r.OverriddenLimits)
	}
}

func TestBuildManifest(t *testing.T) {
	ds, _ := parseDeploymentListJSON([]byte(`{"items":[{"kind":"Deployment",
	  "metadata":{"name":"api","namespace":"n","uid":"x","resourceVersion":"1",
	    "annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}","keep":"me"}},
	  "spec":{"replicas":10,"template":{"spec":{"containers":[
	    {"name":"api","image":"api:1","resources":{"requests":{"cpu":"250m","memory":"256Mi"},"limits":{"cpu":"2","memory":"256Mi"}}}]}}},
	  "status":{"replicas":10}}]}`))
	env := promWith("medium")
	dev, _ := ProfileByKey(DefaultProfiles(), "dev")
	dev.Override = env.Override
	p := PlanDeployment(&ds[0], env, dev) // prom → dev: 10 × 0.2 = 2
	obj, _ := buildManifest(p, dev)

	if _, ok := obj["status"]; ok {
		t.Error("status не удалён")
	}
	md := obj["metadata"].(map[string]interface{})
	if md["uid"] != nil || md["resourceVersion"] != nil {
		t.Error("служебные поля metadata не удалены")
	}
	ann := md["annotations"].(map[string]interface{})
	if ann["keep"] != "me" || ann["kubectl.kubernetes.io/last-applied-configuration"] != nil {
		t.Errorf("annotations = %v", ann)
	}
	if ann["mk8s-calc/original-limits"] != "api cpu 2" {
		t.Errorf("original-limits = %v", ann["mk8s-calc/original-limits"])
	}
	if mapGet(obj, "spec", "replicas") != 2 {
		t.Errorf("replicas = %v", mapGet(obj, "spec", "replicas"))
	}
	c := mapGet(obj, "spec", "template", "spec", "containers").([]interface{})[0].(map[string]interface{})
	if lim := mapGet(c, "resources", "limits").(map[string]interface{}); lim["cpu"] != "375m" || lim["memory"] != "256Mi" {
		t.Errorf("limits = %v (ожидали cpu 375m, memory без изменений)", lim)
	}
	if ds[0].Raw["status"] == nil {
		t.Error("buildManifest изменил исходный Raw")
	}
}
