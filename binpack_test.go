package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planOf(name string, replicas int, reqCPU, reqMem, limCPU, limMem int64) DeploymentPlan {
	return DeploymentPlan{Name: name, Namespace: "t", Replicas: replicas, Containers: []ContainerPlan{{
		Name:     name,
		Requests: ResourceSpec{CPUMilli: reqCPU, MemMiB: reqMem},
		Limits:   ResourceSpec{CPUMilli: limCPU, MemMiB: limMem},
	}}}
}

func envByKey(key string) EnvProfile {
	e, _ := ProfileByKey(DefaultProfiles(), key)
	return e
}

func paramsWith(servers ...ServerConfig) PackingParams {
	p := DefaultPackingParams()
	p.Servers = servers
	return p
}

var bigServer = ServerConfig{Name: "big", Cores: 128, RAMGiB: 1024}

func TestPackPodLimitIsBottleneck(t *testing.T) {
	// 500 крошечных подов: по CPU/RAM влезли бы на 1 сервер, но maxPods=110
	// (минус 6 DaemonSet = 104 слота) требует ceil(500/104) = 5 серверов.
	plans := []DeploymentPlan{planOf("tiny", 500, 10, 32, 20, 64)}
	res := PackEnv(plans, envByKey("dev"), paramsWith(bigServer))
	o := res.Best
	if o == nil || o.NodesPacked != 5 || !strings.Contains(o.Bottleneck, "число подов") {
		t.Fatalf("best = %+v", o)
	}
	if o.MaxPodsOnNode > 104 {
		t.Errorf("на ноде %d подов > 104 слотов", o.MaxPodsOnNode)
	}
}

func TestPackSpreadAndNPlusOne(t *testing.T) {
	// 4 реплики, каждая влезла бы на одну ноду вся; на ПРОМ не больше
	// ceil(4/2) = 2 реплик на ноду → 2 ноды + 1 запасная.
	plans := []DeploymentPlan{planOf("api", 4, 500, 512, 1000, 1024)}
	prom := PackEnv(plans, envByKey("prom"), paramsWith(bigServer)).Best
	if prom.NodesPacked != 2 || prom.SpareNodes != 1 || prom.Nodes != 3 {
		t.Errorf("ПРОМ: %+v", prom)
	}
	// На DEV разнесения и N+1 нет, но минимальный кластер — 2 сервера.
	dev := PackEnv(plans, envByKey("dev"), paramsWith(bigServer)).Best
	if dev.Nodes != 2 || dev.SpareNodes != 0 {
		t.Errorf("DEV: %+v", dev)
	}
}

func TestPackPodTooBig(t *testing.T) {
	small := ServerConfig{Name: "small", Cores: 8, RAMGiB: 32}
	plans := []DeploymentPlan{planOf("huge", 1, 4000, 64*1024, 4000, 64*1024)}
	res := PackEnv(plans, envByKey("dev"), paramsWith(small, bigServer))
	if res.Options[0].Feasible || !strings.Contains(res.Options[0].Reason, "не помещается") {
		t.Errorf("small: %+v", res.Options[0])
	}
	if res.Best == nil || res.Best.Server.Name != "big" {
		t.Errorf("best = %+v", res.Best)
	}
	res = PackEnv(plans, envByKey("dev"), paramsWith(small))
	if res.Best != nil || len(res.Warnings) == 0 {
		t.Errorf("ожидали отсутствие варианта и предупреждение: %+v", res)
	}
}

func TestPackProfilesOrdering(t *testing.T) {
	// Поды с limits ×4 по CPU: уплотнение по limits решает — DWH плотнее OLTP.
	plans := []DeploymentPlan{planOf("svc", 200, 500, 1024, 2000, 1024)}
	cores := map[string]int{}
	for _, key := range []string{"oltp", "mixed", "dwh"} {
		p := DefaultPackingParams()
		p.Profile, _ = ProfileByKeyApp(key)
		cores[key] = PackEnv(plans, envByKey("prom"), p).Best.TotalCores
	}
	if !(cores["dwh"] <= cores["mixed"] && cores["mixed"] <= cores["oltp"]) || cores["dwh"] == cores["oltp"] {
		t.Errorf("ожидали dwh < oltp по ядрам: %v", cores)
	}
}

func TestPackRespectsCaps(t *testing.T) {
	p := DefaultPackingParams()
	plans := []DeploymentPlan{planOf("svc", 300, 700, 2048, 2500, 3072)}
	for _, o := range PackEnv(plans, envByKey("psi"), p).Options {
		if !o.Feasible {
			continue
		}
		if o.CPUReqUtil > 100*p.Profile.CPUReqCap+0.01 || o.CPULimitK > p.Profile.CPULimitK+0.001 || o.MemLimitK > p.Profile.MemLimitK+0.001 {
			t.Errorf("%s: превышены ограничения профиля: req %.1f%%, K cpu %.2f, K mem %.2f", o.Server.Name, o.CPUReqUtil, o.CPULimitK, o.MemLimitK)
		}
	}
}

func TestPackClusterPodLimit(t *testing.T) {
	p := paramsWith(bigServer)
	p.MaxPodsPerCluster = 100
	res := PackEnv([]DeploymentPlan{planOf("x", 150, 10, 32, 10, 32)}, envByKey("dev"), p)
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "несколько кластеров") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestLoadServers(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	os.WriteFile(good, []byte(`[{"name":"s1","cores":64,"ram_gib":512,"disks":"2×NVMe"}]`), 0o644)
	s, err := LoadServers(good)
	if err != nil || len(s) != 1 || s[0].Cores != 64 {
		t.Errorf("s = %+v, err = %v", s, err)
	}
	for _, cfg := range []string{`{"name":"s1","cores":0,"ram_gib":512}`, `{"name":"s2","cores":16,"ram_gib":64}`, `{"name":"s3","cores":32,"ram_gib":32}`} {
		bad := filepath.Join(dir, "bad.json")
		os.WriteFile(bad, []byte("["+cfg+"]"), 0o644)
		if _, err := LoadServers(bad); err == nil || !strings.Contains(err.Error(), "минимального барика") {
			t.Errorf("%s: ожидали отказ «меньше минимального барика», err = %v", cfg, err)
		}
	}
}

func TestBaremetalVerdict(t *testing.T) {
	p := DefaultPackingParams() // mixed: CPU req cap 0.75, limit K 2.0; мин. кластер 2 × 32/64
	// Минимальный кластер: allocatable CPU = 2 × (32000 − 1320 − 600) = 60.16 ядра.
	small := []DeploymentPlan{planOf("app", 10, 1000, 2048, 2000, 2048)} // нужно max(10/0.75, 20/2) = 13.3 ядра
	v := PackEnv(small, envByKey("prom"), p).Verdict
	if v.Worthwhile || !strings.Contains(v.Message, "нецелесообразен") || v.NeedCores < 13.3 || v.NeedCores > 13.4 {
		t.Errorf("small: %+v", v)
	}
	big := []DeploymentPlan{planOf("app", 60, 1000, 2048, 2000, 2048)} // 80 ядер > 60.16
	if v := PackEnv(big, envByKey("prom"), p).Verdict; !v.Worthwhile {
		t.Errorf("big: %+v", v)
	}
	// Решает и память: мало CPU, но много RAM.
	memHeavy := []DeploymentPlan{planOf("cache", 4, 500, 32*1024, 500, 32*1024)}
	if v := PackEnv(memHeavy, envByKey("prom"), p).Verdict; !v.Worthwhile {
		t.Errorf("memHeavy: %+v", v)
	}
}

func TestPackUsesPoolAvailability(t *testing.T) {
	p := DefaultPackingParams()
	p.Servers = []ServerConfig{
		{Name: "small", Cores: 32, RAMGiB: 256, Available: 2}, // меньше ядер, но в пуле мало
		{Name: "big", Cores: 48, RAMGiB: 1024, Available: 20},
	}
	plans := []DeploymentPlan{planOf("svc", 60, 1000, 2048, 2000, 2048)}
	res := PackEnv(plans, envByKey("prom"), p)
	if res.Best == nil || res.Best.Server.Name != "big" || res.Best.PoolShortBy != 0 {
		t.Fatalf("ожидали тип, которого хватает в пуле: %+v", res.Best)
	}
	if res.Options[0].PoolShortBy == 0 {
		t.Errorf("small: ожидали нехватку в пуле, %+v", res.Options[0])
	}
	// Никого не хватает — выбирается тип с наименьшей нехваткой.
	p.Servers[1].Available = 2
	res = PackEnv(plans, envByKey("prom"), p)
	short := map[string]int{}
	for _, o := range res.Options {
		short[o.Server.Name] = o.PoolShortBy
	}
	want := "small"
	if short["big"] < short["small"] {
		want = "big"
	}
	if res.Best.Server.Name != want || len(res.Warnings) == 0 {
		t.Errorf("best = %s (нехватки %v), warnings = %v", res.Best.Server.Name, short, res.Warnings)
	}
}

func TestInventoryTypesCatalog(t *testing.T) {
	inv := []InventoryServer{
		invServer("A1", 32, 768, true), invServer("A2", 32, 768, true), invServer("A3", 32, 768, false),
		invServer("B1", 48, 1024, true), invServer("T", 16, 64, true),
	}
	types := InventoryTypes(inv, DefaultPackingParams())
	if len(types) != 2 || types[0].Cores != 32 || types[0].Available != 2 || types[1].Available != 1 {
		t.Errorf("types = %+v", types)
	}
	p := DefaultPackingParams()
	p.UseInventoryCatalog(InventorySource{Path: "x.csv", Servers: inv})
	if len(p.Servers) != 2 || !strings.Contains(p.ServersSource, "x.csv") {
		t.Errorf("catalog = %+v, source = %q", p.Servers, p.ServersSource)
	}
}
