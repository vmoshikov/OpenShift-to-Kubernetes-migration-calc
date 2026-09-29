package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Строки ровно в формате пользователя: params с удвоенными кавычками,
// числа и числами, и строками.
const userInventory = `MANAGED_K8S,0,CI02690156,a09lclq2,true,X86_R_2S_SDS_GENERAL_32,Huawei,Huawei 2288H V5,"{""cpu"": 32, ""hdd"": 72000, ""ram"": 768, ""unit"": 0, ""hdd_type"": ""NVMe+SSD+HDD"", ""lan1025g"": 4}"
MANAGED_K8S,0,CI02381430,a633rn87,false,X86_R_2S_SDS_GENERAL_32,Huawei,Huawei 2288H V5,"{""cpu"": 32, ""hdd"": 72000, ""ram"": 768, ""unit"": 0, ""power"": 530, ""hdd_type"": ""HDD+SDD+NVMe"", ""lan1025g"": 4}"
MANAGED_K8S,0,CI03533962,bcwgr61l,true,X86_R_2S_VSAN_GENERAL_48,Lenovo,Lenovo ThinkSystem SR650,"{""cpu"": 48, ""hdd"": ""15000"", ""ram"": ""1024"", ""power"": 580, ""hdd_type"": ""NVMe"", ""lan1025g"": 4}"
MANAGED_K8S,0,CI03807647,a9mkbrwt,false,X86_R_4S_OST_96,Lenovo,Lenovo ThinkSystem SR850 V2,"{""cpu"": 96, ""hdd"": ""960"", ""ram"": ""3072"", ""san"": 2, ""power"": 890, ""hdd_type"": ""SSD"", ""lan1025g"": 2, ""san_type"": [""16G""]}"
`

func TestParseInventoryUserFormat(t *testing.T) {
	inv, warns, err := ParseInventoryCSV(strings.NewReader(userInventory))
	if err != nil || len(warns) != 0 || len(inv) != 4 {
		t.Fatalf("inv=%d warns=%v err=%v", len(inv), warns, err)
	}
	s := inv[2]
	if s.NodeCI != "CI03533962" || s.ComputeID != "bcwgr61l" || !s.Available || s.Cores != 48 || s.RAMGiB != 1024 || s.DiskGB != 15000 || s.DiskType != "NVMe" {
		t.Errorf("SR650 = %+v", s)
	}
	if inv[1].Available || inv[3].Cores != 96 || inv[3].RAMGiB != 3072 {
		t.Errorf("rows = %+v / %+v", inv[1], inv[3])
	}
	// С заголовком — тот же результат.
	withHdr := "product,node_id,node_ci,node_compute_id,available,code,vendor_title,model_title,params\n" + userInventory
	if inv2, _, _ := ParseInventoryCSV(strings.NewReader(withHdr)); len(inv2) != 4 || inv2[0].NodeCI != "CI02690156" {
		t.Errorf("с заголовком: %+v", inv2)
	}
	// Битые params — предупреждение, строка пропущена.
	_, warns, _ = ParseInventoryCSV(strings.NewReader(`MANAGED_K8S,0,CIX,x,true,C,V,M,not-json` + "\n"))
	if len(warns) != 1 {
		t.Errorf("warns = %v", warns)
	}
}

func invServer(ci string, cores, ram int, avail bool) InventoryServer {
	return InventoryServer{NodeCI: ci, ComputeID: "id-" + ci, Available: avail, Code: fmt.Sprintf("T%d_%d", cores, ram), Cores: cores, RAMGiB: ram}
}

func resultFor(env string, plans ...DeploymentPlan) EnvSizingResult {
	return EnvSizingResult{Env: envByKey(env), Deployments: plans}
}

func TestPlaceFiltersAndHomogeneous(t *testing.T) {
	inv := []InventoryServer{
		invServer("A1", 32, 768, true), invServer("A2", 32, 768, true), invServer("A3", 32, 768, true),
		invServer("OFF", 32, 768, false), invServer("TINY", 16, 64, true),
	}
	pl := PlaceOnInventory([]EnvSizingResult{resultFor("dev", planOf("svc", 4, 1000, 2048, 2000, 2048))}, inv, nil, DefaultPackingParams())
	if pl.PoolTotal != 3 || len(pl.Skipped) != 2 {
		t.Fatalf("pool=%d skipped=%v", pl.PoolTotal, pl.Skipped)
	}
	e := pl.Envs[0]
	if e.Option == nil || e.Mixed || len(e.Servers) != 2 { // минимальный кластер 2
		t.Fatalf("dev = %+v", e)
	}
	for _, s := range e.Servers {
		if s.NodeCI == "OFF" || s.NodeCI == "TINY" {
			t.Errorf("взят недоступный/малый сервер %s", s.NodeCI)
		}
	}
	if len(pl.Remaining) != 1 || pl.Remaining[0].Count != 1 {
		t.Errorf("remaining = %+v", pl.Remaining)
	}
}

func TestPlaceNoDoubleBookingAndPriority(t *testing.T) {
	var inv []InventoryServer
	for i := 0; i < 5; i++ {
		inv = append(inv, invServer(fmt.Sprintf("S%d", i), 32, 256, true))
	}
	plan := planOf("svc", 4, 1000, 2048, 2000, 2048)
	// ПРОМ (2 + N+1 = 3) и DEV (2) — ровно 5 серверов; ещё IFT (2) не влезает.
	pl := PlaceOnInventory([]EnvSizingResult{resultFor("dev", plan), resultFor("ift", plan), resultFor("prom", plan)}, inv, nil, DefaultPackingParams())
	seen := map[string]string{}
	for _, e := range pl.Envs {
		for _, s := range e.Servers {
			if prev, ok := seen[s.NodeCI]; ok {
				t.Errorf("%s выделен и в %s, и в %s", s.NodeCI, prev, e.Env)
			}
			seen[s.NodeCI] = e.Env
		}
	}
	byEnv := map[string]EnvPlacement{}
	for _, e := range pl.Envs {
		byEnv[e.Env] = e
	}
	if len(byEnv["prom"].Servers) != 3 {
		t.Errorf("ПРОМ должен получить серверы первым: %+v", byEnv["prom"])
	}
	if byEnv["dev"].Shortage == "" || !strings.Contains(byEnv["dev"].Shortage, "нужно ещё ≈2") {
		t.Errorf("DEV (последний по приоритету) — ожидали нехватку ≈2: %q", byEnv["dev"].Shortage)
	}
	if pl.Envs[0].Env != "dev" {
		t.Errorf("вывод — в исходном порядке контуров")
	}
}

func TestPlaceMixedSpareIsLargest(t *testing.T) {
	// Ни одного типа не хватает на ПРОМ однородно → смешанный набор;
	// запасной N+1 — самый крупный сервер.
	inv := []InventoryServer{
		invServer("BIG1", 96, 1024, true), invServer("BIG2", 96, 1024, true),
		invServer("M1", 48, 512, true), invServer("M2", 48, 512, true),
	}
	plan := planOf("svc", 40, 2000, 4096, 4000, 4096) // ~80 ядер requests
	pl := PlaceOnInventory([]EnvSizingResult{resultFor("prom", plan)}, inv, nil, DefaultPackingParams())
	e := pl.Envs[0]
	if e.Option == nil || !e.Mixed || e.Option.SpareNodes != 1 {
		t.Fatalf("prom = %+v / %+v", e, e.Option)
	}
	spare := e.Servers[len(e.Servers)-1]
	if spare.Cores != 96 {
		t.Errorf("запасной N+1 = %s (%d ядер), ожидали самый крупный", spare.NodeCI, spare.Cores)
	}
	cores := 0
	for _, s := range e.Servers {
		cores += s.Cores
	}
	if cores != e.Option.TotalCores || len(e.Servers) != e.Option.Nodes {
		t.Errorf("итоги не совпадают с серверами: %d ядер vs %d, %d серв. vs %d", cores, e.Option.TotalCores, len(e.Servers), e.Option.Nodes)
	}
}

func TestPlacementCSV(t *testing.T) {
	inv := []InventoryServer{invServer("A1", 32, 256, true), invServer("A2", 32, 256, true)}
	pl := PlaceOnInventory([]EnvSizingResult{resultFor("dev", planOf("svc", 2, 500, 512, 1000, 512))}, inv, nil, DefaultPackingParams())
	var buf bytes.Buffer
	if err := WritePlacementCSV(&buf, pl); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "dev,A1,id-A1,") {
		t.Errorf("csv =\n%s", buf.String())
	}
}
