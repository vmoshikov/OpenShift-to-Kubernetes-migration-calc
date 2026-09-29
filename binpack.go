package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
)

// Подбор baremetal-серверов и коэффициента уплотнения по раскладке подов
// (bin-packing), а не по суммарному V. Для каждой конфигурации сервера из
// каталога поды контура раскладываются First-Fit Decreasing с учётом:
//   - requests ≤ allocatable × потолок утилизации профиля (планировщик K8s
//     размещает по requests; потолок — запас на пики/задержки);
//   - Σ limits ≤ allocatable × коэффициент уплотнения профиля (сколько
//     «обещанного» ресурса можно положить на ноду без риска);
//   - число подов ≤ maxPods (kubelet, по умолчанию 110), включая DaemonSet;
//   - системного резерва ноды (kubelet/system/eviction) и DaemonSet;
//   - разнесения реплик: на ПСИ/ПРОМ не больше ceil(replicas/2) реплик
//     одного сервиса на ноду — потеря ноды не убивает сервис;
//   - N+1 на ПСИ/ПРОМ.
// Результат — конкретная конфигурация серверов, их число и достигнутый
// коэффициент уплотнения.
//
// TODO(prometheus): «безопасность» уплотнения сейчас задаётся профилем
// (стартовые коэффициенты). С метриками — проверять Σ p99 потребления
// подов на ноде ≤ allocatable × потолок, и подбирать коэффициент по факту.

// ServerConfig — конфигурация baremetal-сервера.
type ServerConfig struct {
	Name      string `json:"name"`
	Cores     int    `json:"cores"`
	RAMGiB    int    `json:"ram_gib"`
	Disks     string `json:"disks,omitempty"`     // для вывода; дисковая подсистема не считается
	Model     string `json:"model,omitempty"`     // вендор и модель (из пула)
	Available int    `json:"available,omitempty"` // сколько таких в пуле; 0 — каталог без ограничения
}

// DefaultServers — стартовый каталог. Условные типовые 2-сокетные серверы;
// в реальном расчёте подставить каталог DropApp Baremetal (-servers=file.json).
func DefaultServers() []ServerConfig {
	return []ServerConfig{
		{Name: "bm-32c-64g", Cores: 32, RAMGiB: 64, Disks: "2×480GB SSD"}, // минимальный барик
		{Name: "bm-32c-128g", Cores: 32, RAMGiB: 128, Disks: "2×960GB NVMe"},
		{Name: "bm-32c-256g", Cores: 32, RAMGiB: 256, Disks: "2×960GB NVMe"},
		{Name: "bm-48c-384g", Cores: 48, RAMGiB: 384, Disks: "2×1.92TB NVMe"},
		{Name: "bm-64c-512g", Cores: 64, RAMGiB: 512, Disks: "2×1.92TB NVMe"},
		{Name: "bm-64c-1024g", Cores: 64, RAMGiB: 1024, Disks: "4×1.92TB NVMe"},
		{Name: "bm-96c-768g", Cores: 96, RAMGiB: 768, Disks: "4×1.92TB NVMe"},
		{Name: "bm-128c-1024g", Cores: 128, RAMGiB: 1024, Disks: "4×3.84TB NVMe"},
	}
}

// LoadServers читает каталог серверов из JSON-массива ServerConfig.
func LoadServers(path string) ([]ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s []ServerConfig
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("каталог серверов %s: %w", path, err)
	}
	min := DefaultPackingParams().MinServer
	for _, c := range s {
		if c.Cores < min.Cores || c.RAMGiB < min.RAMGiB {
			return nil, fmt.Errorf("каталог серверов: %q (%d ядер / %d GiB) меньше минимального барика %d/%d",
				c.Name, c.Cores, c.RAMGiB, min.Cores, min.RAMGiB)
		}
	}
	return s, nil
}

// AppProfile — профиль приложения: насколько плотно можно укладывать поды.
type AppProfile struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	CPUReqCap float64 `json:"cpu_request_cap"` // доля allocatable CPU под requests
	MemReqCap float64 `json:"mem_request_cap"`
	CPULimitK float64 `json:"cpu_limit_overcommit"` // Σ CPU limits / allocatable CPU
	MemLimitK float64 `json:"mem_limit_overcommit"` // Σ memory limits / allocatable RAM
	Note      string  `json:"note"`
}

// AppProfiles — стартовые коэффициенты; калибровать по p95/p99.
// Память не переподписывается ни в одном профиле всерьёз: превышение по
// памяти — это OOMKill, а не замедление, как у CPU.
var AppProfiles = []AppProfile{
	{"oltp", "OLTP", 0.65, 0.85, 1.5, 1.0,
		"чувствителен к задержкам: низкий потолок CPU, мягкое уплотнение по limits"},
	{"mixed", "Смешанный", 0.75, 0.85, 2.0, 1.0,
		"умеренное уплотнение"},
	{"dwh", "DWH / пакетный", 0.85, 0.90, 3.0, 1.1,
		"пакетная нагрузка терпит очередь за CPU: жёсткое уплотнение по CPU"},
}

// ProfileByKeyApp ищет профиль приложения.
func ProfileByKeyApp(key string) (AppProfile, error) {
	for _, p := range AppProfiles {
		if p.Key == key {
			return p, nil
		}
	}
	return AppProfile{}, fmt.Errorf("неизвестный профиль %q (ожидается oltp | mixed | dwh)", key)
}

// PackingParams — ограничения K8s и резервы ноды.
type PackingParams struct {
	Profile           AppProfile     `json:"profile"`
	Servers           []ServerConfig `json:"servers"`
	ServersSource     string         `json:"servers_source"`       // откуда типы серверов: пул CSV / -servers / встроенный
	MaxPodsPerNode    int            `json:"max_pods_per_node"`    // kubelet maxPods
	MaxPodsPerCluster int            `json:"max_pods_per_cluster"` // ограничение etcd/API-сервера
	DaemonSetPods     int            `json:"daemonset_pods"`       // DaemonSet на каждой ноде (CNI, CSI, логи, мониторинг)
	DaemonSetCPU      int64          `json:"daemonset_cpu_milli"`
	DaemonSetMemMiB   int64          `json:"daemonset_mem_mib"`
	MinClusterNodes   int            `json:"min_cluster_nodes"` // минимальный кластер
	MinServer         ServerConfig   `json:"min_server"`        // минимальный барик
}

// DefaultPackingParams — дефолты. MaxPodsPerCluster — порядок величины для
// одного кластера с etcd ~6–8 GB; уточнить для DropApp.
func DefaultPackingParams() PackingParams {
	return PackingParams{
		Profile:           AppProfiles[1],
		Servers:           DefaultServers(),
		ServersSource:     "встроенный условный каталог (нет файла пула серверов)",
		MaxPodsPerNode:    110,
		MaxPodsPerCluster: 30000,
		DaemonSetPods:     6,
		DaemonSetCPU:      600,
		DaemonSetMemMiB:   1536,
		MinClusterNodes:   2,
		MinServer:         ServerConfig{Name: "bm-32c-64g", Cores: 32, RAMGiB: 64},
	}
}

// nodeAllocatable — ёмкость ноды под поды приложения: минус системный
// резерв (kube-reserved + system-reserved + eviction; ~1 ядро + 1% и
// 4 GiB + 2%) и DaemonSet.
func nodeAllocatable(s ServerConfig, p PackingParams) (cpu, mem int64) {
	capCPU := int64(s.Cores) * 1000
	capMem := int64(s.RAMGiB) * 1024
	cpu = capCPU - (1000 + capCPU/100) - p.DaemonSetCPU
	mem = capMem - (4096 + capMem*2/100) - p.DaemonSetMemMiB
	return
}

type packPod struct {
	app            string
	reqCPU, reqMem int64
	limCPU, limMem int64
}

type packNode struct {
	server                         ServerConfig
	caps                           nodeCaps
	reqCPU, reqMem, limCPU, limMem int64
	pods                           int
	perApp                         map[string]int
}

// nodeCaps — пределы одной ноды по правилам профиля.
type nodeCaps struct {
	allocCPU, allocMem int64
	reqCPU, reqMem     int64 // allocatable × потолок утилизации
	limCPU, limMem     int64 // allocatable × коэффициент уплотнения
	pods               int   // слотов под поды приложения (maxPods − DaemonSet)
}

func capsFor(s ServerConfig, p PackingParams) (nodeCaps, bool) {
	cpu, mem := nodeAllocatable(s, p)
	c := nodeCaps{
		allocCPU: cpu, allocMem: mem,
		reqCPU: int64(float64(cpu) * p.Profile.CPUReqCap), reqMem: int64(float64(mem) * p.Profile.MemReqCap),
		limCPU: int64(float64(cpu) * p.Profile.CPULimitK), limMem: int64(float64(mem) * p.Profile.MemLimitK),
		pods: p.MaxPodsPerNode - p.DaemonSetPods,
	}
	return c, cpu > 0 && mem > 0 && c.pods > 0
}

// serverSupply выдаёт серверы для новых нод: для каталога — бесконечно один
// тип, для инвентаря — конечный список конкретных серверов.
type serverSupply interface {
	next() (ServerConfig, bool)
}

type unlimitedSupply struct{ s ServerConfig }

func (u unlimitedSupply) next() (ServerConfig, bool) { return u.s, true }

type listSupply struct {
	servers []ServerConfig
	used    int
}

func (l *listSupply) next() (ServerConfig, bool) {
	if l.used >= len(l.servers) {
		return ServerConfig{}, false
	}
	l.used++
	return l.servers[l.used-1], true
}

// PackingOption — результат раскладки на одной конфигурации сервера.
type PackingOption struct {
	Server        ServerConfig  `json:"server"`
	Feasible      bool          `json:"feasible"`
	Reason        string        `json:"reason,omitempty"`
	NodesPacked   int           `json:"nodes_packed"`
	SpareNodes    int           `json:"spare_nodes"` // N+1
	Nodes         int           `json:"nodes"`
	TotalCores    int           `json:"total_cores"`
	TotalRAMGiB   int           `json:"total_ram_gib"`
	CPUReqUtil    float64       `json:"cpu_request_util_pct"` // Σ requests / Σ allocatable (без запасной ноды)
	MemReqUtil    float64       `json:"mem_request_util_pct"`
	CPULimitK     float64       `json:"cpu_limit_overcommit"` // достигнутый коэффициент уплотнения
	MemLimitK     float64       `json:"mem_limit_overcommit"`
	PodsPerNode   float64       `json:"pods_per_node_avg"`
	MaxPodsOnNode int           `json:"pods_per_node_max"`
	Bottleneck    string        `json:"bottleneck"`              // что ограничивает укладку
	Composition   []ServerCount `json:"composition,omitempty"`   // состав (для смешанного набора — несколько типов)
	PoolShortBy   int           `json:"pool_short_by,omitempty"` // сколько серверов этого типа не хватает в пуле
}

// PackingResult — подбор серверов для контура.
type PackingResult struct {
	Pods          int              `json:"pods"`
	PodStats      PodStats         `json:"pod_stats"`
	Options       []PackingOption  `json:"options"`
	Best          *PackingOption   `json:"best,omitempty"`
	ServersSource string           `json:"servers_source"`
	Verdict       BaremetalVerdict `json:"verdict"`
	Warnings      []string         `json:"warnings,omitempty"`
}

// PodStats — распределение размеров подов (по requests) и характер нагрузки.
type PodStats struct {
	CPUP50Milli int64   `json:"cpu_p50_milli"`
	CPUP95Milli int64   `json:"cpu_p95_milli"`
	CPUMaxMilli int64   `json:"cpu_max_milli"`
	MemP50MiB   int64   `json:"mem_p50_mib"`
	MemP95MiB   int64   `json:"mem_p95_mib"`
	MemMaxMiB   int64   `json:"mem_max_mib"`
	GiBPerCore  float64 `json:"gib_per_core"` // Σ mem requests / Σ CPU requests
	ProfileHint string  `json:"profile_hint"`
}

// PackEnv раскладывает поды контура на каждую конфигурацию из каталога.
func PackEnv(plans []DeploymentPlan, env EnvProfile, p PackingParams) PackingResult {
	pods, replicas := podsOf(plans)
	res := PackingResult{Pods: len(pods), PodStats: podStats(pods), ServersSource: p.ServersSource}

	ha := env.MinReplicas >= 2 // ПСИ/ПРОМ: разнесение реплик и N+1
	if len(pods) > p.MaxPodsPerCluster {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d подов > %d на кластер (ограничение etcd/API-сервера) — делить на несколько кластеров", len(pods), p.MaxPodsPerCluster))
	}

	res.Verdict = baremetalVerdict(pods, p)

	for _, s := range p.Servers {
		res.Options = append(res.Options, packOn(pods, replicas, s, p, ha))
	}
	// Сначала варианты, которых хватает в пуле; если ни одного — лучший из
	// нехватающих (сколько докупить видно по PoolShortBy).
	for pass := 0; pass < 2 && res.Best == nil; pass++ {
		for i := range res.Options {
			o := &res.Options[i]
			if !o.Feasible || (pass == 0 && o.PoolShortBy > 0) {
				continue
			}
			// При нехватке — тот, которого не хватает меньше всего.
			if res.Best == nil || o.PoolShortBy < res.Best.PoolShortBy ||
				(o.PoolShortBy == res.Best.PoolShortBy && betterPacking(*o, *res.Best)) {
				res.Best = o
			}
		}
	}
	if res.Best == nil && len(pods) > 0 {
		res.Warnings = append(res.Warnings, "ни один тип сервера не вмещает крупнейший под")
	}
	if res.Best != nil && res.Best.PoolShortBy > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("ни одного типа сервера не хватает в пуле однородно; ближе всего %s — не хватает %d шт. (см. размещение на пуле — смешанный набор)",
			res.Best.Server.Name, res.Best.PoolShortBy))
	}
	return res
}

// betterPacking: меньше ядер всего → меньше RAM всего → меньше серверов.
// Без прайса «дешевле» ≈ «меньше железа».
func betterPacking(a, b PackingOption) bool {
	if a.TotalCores != b.TotalCores {
		return a.TotalCores < b.TotalCores
	}
	if a.TotalRAMGiB != b.TotalRAMGiB {
		return a.TotalRAMGiB < b.TotalRAMGiB
	}
	return a.Nodes < b.Nodes
}

func packOn(pods []packPod, replicas map[string]int, s ServerConfig, p PackingParams, ha bool) PackingOption {
	opt, _ := packServers(pods, replicas, unlimitedSupply{s}, s, p, ha, ha)
	opt.Server = s
	if opt.Feasible && s.Available > 0 && opt.Nodes > s.Available {
		opt.PoolShortBy = opt.Nodes - s.Available
	}
	return opt
}

// packServers раскладывает поды First-Fit Decreasing, открывая новые ноды на
// серверах из supply; ref — сервер, относительно которого упорядочиваются
// поды (крупные первыми). Возвращает итог и использованные серверы (в том
// числе добавленные до минимального кластера и N+1).
// addSpare — добавить запасной сервер N+1 из того же supply (однородный
// набор); для смешанного набора запасной выбирается снаружи — самый крупный.
func packServers(pods []packPod, replicas map[string]int, supply serverSupply, ref ServerConfig, p PackingParams, ha, addSpare bool) (PackingOption, []ServerConfig) {
	var opt PackingOption
	refCaps, ok := capsFor(ref, p)
	if !ok {
		opt.Reason = "сервер меньше системного резерва"
		return opt, nil
	}

	// FFD: крупные (по доминирующей доле ресурса ноды) — первыми.
	sorted := append([]packPod(nil), pods...)
	share := func(x packPod) float64 {
		return math.Max(
			math.Max(float64(x.reqCPU)/float64(refCaps.reqCPU), float64(x.reqMem)/float64(refCaps.reqMem)),
			math.Max(float64(x.limCPU)/float64(refCaps.limCPU), float64(x.limMem)/float64(refCaps.limMem)))
	}
	sort.SliceStable(sorted, func(i, j int) bool { return share(sorted[i]) > share(sorted[j]) })

	maxPerNode := func(app string) int {
		if !ha || replicas[app] < 2 {
			return math.MaxInt
		}
		return (replicas[app] + 1) / 2 // ceil(r/2): потеря ноды оставляет ≥ половины реплик
	}
	fits := func(n *packNode, x packPod) bool {
		c := n.caps
		return n.reqCPU+x.reqCPU <= c.reqCPU && n.reqMem+x.reqMem <= c.reqMem &&
			n.limCPU+x.limCPU <= c.limCPU && n.limMem+x.limMem <= c.limMem &&
			n.pods+1 <= c.pods && n.perApp[x.app] < maxPerNode(x.app)
	}
	open := func() (*packNode, bool) {
		s, ok := supply.next()
		if !ok {
			return nil, false
		}
		c, ok := capsFor(s, p)
		if !ok {
			return nil, false
		}
		return &packNode{server: s, caps: c, perApp: map[string]int{}}, true
	}

	var nodes []*packNode
	for _, x := range sorted {
		placed := false
		for _, n := range nodes {
			if fits(n, x) {
				n.add(x)
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		n, ok := open()
		if !ok {
			opt.Reason = fmt.Sprintf("серверы кончились на %d-м, не все поды размещены", len(nodes)+1)
			return opt, nil
		}
		if !fits(n, x) {
			opt.Reason = fmt.Sprintf("под %s (req %dm/%dMi, lim %dm/%dMi) не помещается на пустой сервер %s",
				x.app, x.reqCPU, x.reqMem, x.limCPU, x.limMem, n.server.Name)
			return opt, nil
		}
		n.add(x)
		nodes = append(nodes, n)
	}

	// Минимальный кластер — на любом контуре.
	for len(nodes) < p.MinClusterNodes {
		n, ok := open()
		if !ok {
			opt.Reason = fmt.Sprintf("не хватает серверов до минимального кластера (%d)", p.MinClusterNodes)
			return opt, nil
		}
		nodes = append(nodes, n)
	}
	used := make([]ServerConfig, 0, len(nodes)+1)
	for _, n := range nodes {
		used = append(used, n.server)
	}
	// N+1: в однородном наборе запасной — такой же сервер, значит он примет
	// нагрузку любой упавшей ноды.
	if addSpare {
		n, ok := open()
		if !ok {
			opt.Reason = "не хватает сервера под N+1"
			return opt, nil
		}
		opt.SpareNodes = 1
		used = append(used, n.server)
	}

	opt.Feasible = true
	opt.NodesPacked = len(nodes)
	opt.Nodes = len(used)
	var sum packNode
	var allocCPU, allocMem, capReqCPU, capReqMem, capLimCPU, capLimMem int64
	slots := 0
	for _, n := range nodes {
		sum.reqCPU += n.reqCPU
		sum.reqMem += n.reqMem
		sum.limCPU += n.limCPU
		sum.limMem += n.limMem
		allocCPU += n.caps.allocCPU
		allocMem += n.caps.allocMem
		capReqCPU += n.caps.reqCPU
		capReqMem += n.caps.reqMem
		capLimCPU += n.caps.limCPU
		capLimMem += n.caps.limMem
		slots += n.caps.pods
		if n.pods > opt.MaxPodsOnNode {
			opt.MaxPodsOnNode = n.pods
		}
	}
	for _, s := range used {
		opt.TotalCores += s.Cores
		opt.TotalRAMGiB += s.RAMGiB
	}
	opt.Composition = composition(used)
	opt.CPUReqUtil = 100 * float64(sum.reqCPU) / float64(allocCPU)
	opt.MemReqUtil = 100 * float64(sum.reqMem) / float64(allocMem)
	opt.CPULimitK = float64(sum.limCPU) / float64(allocCPU)
	opt.MemLimitK = float64(sum.limMem) / float64(allocMem)
	opt.PodsPerNode = float64(len(pods)) / float64(len(nodes))

	// Узкое место — ограничение, заполненное сильнее всего в среднем.
	type lim struct {
		name string
		fill float64
	}
	fills := []lim{
		{"CPU requests", float64(sum.reqCPU) / float64(capReqCPU)},
		{"RAM requests", float64(sum.reqMem) / float64(capReqMem)},
		{"CPU limits (уплотнение)", float64(sum.limCPU) / float64(capLimCPU)},
		{"RAM limits (уплотнение)", float64(sum.limMem) / float64(capLimMem)},
		{fmt.Sprintf("число подов (%d/нода)", p.MaxPodsPerNode), float64(len(pods)) / float64(slots)},
	}
	best := fills[0]
	for _, f := range fills[1:] {
		if f.fill > best.fill {
			best = f
		}
	}
	opt.Bottleneck = best.name
	if best.fill < 0.5 {
		// Ни одно ограничение не заполнено и наполовину — серверов больше,
		// чем нужно по ресурсам: их число задано минимумом/HA, а не нагрузкой.
		opt.Bottleneck = "стенд меньше сервера (число задано минимумом)"
		if ha {
			opt.Bottleneck = "разнесение реплик / минимум серверов HA"
		}
	}
	return opt, used
}

// ServerCount — сколько серверов одного типа в решении.
type ServerCount struct {
	Server ServerConfig `json:"server"`
	Count  int          `json:"count"`
}

func composition(used []ServerConfig) []ServerCount {
	var out []ServerCount
	idx := map[string]int{}
	for _, s := range used {
		if i, ok := idx[s.Name]; ok {
			out[i].Count++
			continue
		}
		idx[s.Name] = len(out)
		out = append(out, ServerCount{Server: s, Count: 1})
	}
	return out
}

func (n *packNode) add(x packPod) {
	n.reqCPU += x.reqCPU
	n.reqMem += x.reqMem
	n.limCPU += x.limCPU
	n.limMem += x.limMem
	n.pods++
	n.perApp[x.app]++
}

func podStats(pods []packPod) PodStats {
	if len(pods) == 0 {
		return PodStats{}
	}
	cpu := make([]int64, len(pods))
	mem := make([]int64, len(pods))
	var sumCPU, sumMem int64
	for i, x := range pods {
		cpu[i], mem[i] = x.reqCPU, x.reqMem
		sumCPU += x.reqCPU
		sumMem += x.reqMem
	}
	sort.Slice(cpu, func(i, j int) bool { return cpu[i] < cpu[j] })
	sort.Slice(mem, func(i, j int) bool { return mem[i] < mem[j] })
	q := func(s []int64, f float64) int64 { return s[int(math.Ceil(f*float64(len(s))))-1] }
	st := PodStats{
		CPUP50Milli: q(cpu, 0.5), CPUP95Milli: q(cpu, 0.95), CPUMaxMilli: cpu[len(cpu)-1],
		MemP50MiB: q(mem, 0.5), MemP95MiB: q(mem, 0.95), MemMaxMiB: mem[len(mem)-1],
	}
	if sumCPU > 0 {
		st.GiBPerCore = (float64(sumMem) / 1024) / (float64(sumCPU) / 1000)
	}
	// Подсказка по профилю — только по форме ресурсов; характер нагрузки
	// (задержки vs пакетная) по манифестам не определить — выбирает человек.
	switch {
	case st.GiBPerCore >= 8:
		st.ProfileHint = "памятеёмкая нагрузка (характерно для DWH/кэшей/БД) — ноды с RAM ×8 и выше на ядро"
	case st.GiBPerCore <= 2.5 && st.CPUP50Milli <= 500:
		st.ProfileHint = "много мелких CPU-подов (характерно для OLTP/API) — следить за лимитом подов на ноду"
	default:
		st.ProfileHint = "сбалансированная нагрузка"
	}
	return st
}

// BaremetalVerdict — имеет ли смысл baremetal для контура. Потребность —
// сколько allocatable-ресурса нужно, чтобы уложить поды по правилам профиля:
// max(Σ requests / потолок утилизации, Σ limits / коэффициент уплотнения).
// Если она меньше ёмкости минимального baremetal-кластера (MinClusterNodes ×
// MinServer), клиент заплатит за железо, которое не загрузит, — это стоит
// сказать сразу и предложить виртуальный managed K8s.
type BaremetalVerdict struct {
	Worthwhile    bool    `json:"worthwhile"`
	NeedCores     float64 `json:"need_cores"`
	NeedRAMGiB    float64 `json:"need_ram_gib"`
	MinCores      float64 `json:"min_cluster_alloc_cores"` // allocatable минимального кластера
	MinRAMGiB     float64 `json:"min_cluster_alloc_ram_gib"`
	FillCPUPct    float64 `json:"min_cluster_cpu_fill_pct"`
	FillRAMPct    float64 `json:"min_cluster_ram_fill_pct"`
	MinClusterStr string  `json:"min_cluster"`
	Message       string  `json:"message"`
}

func baremetalVerdict(pods []packPod, p PackingParams) BaremetalVerdict {
	var reqCPU, reqMem, limCPU, limMem int64
	for _, x := range pods {
		reqCPU += x.reqCPU
		reqMem += x.reqMem
		limCPU += x.limCPU
		limMem += x.limMem
	}
	needCPU := math.Max(float64(reqCPU)/p.Profile.CPUReqCap, float64(limCPU)/p.Profile.CPULimitK)
	needMem := math.Max(float64(reqMem)/p.Profile.MemReqCap, float64(limMem)/p.Profile.MemLimitK)
	allocCPU, allocMem := nodeAllocatable(p.MinServer, p)
	n := float64(p.MinClusterNodes)

	v := BaremetalVerdict{
		NeedCores:     needCPU / 1000,
		NeedRAMGiB:    needMem / 1024,
		MinCores:      n * float64(allocCPU) / 1000,
		MinRAMGiB:     n * float64(allocMem) / 1024,
		MinClusterStr: fmt.Sprintf("%d × %d ядер / %d GiB", p.MinClusterNodes, p.MinServer.Cores, p.MinServer.RAMGiB),
	}
	v.FillCPUPct = 100 * v.NeedCores / v.MinCores
	v.FillRAMPct = 100 * v.NeedRAMGiB / v.MinRAMGiB
	v.Worthwhile = v.NeedCores >= v.MinCores || v.NeedRAMGiB >= v.MinRAMGiB
	if v.Worthwhile {
		v.Message = fmt.Sprintf("потребность %.1f ядер / %.0f GiB не меньше минимального baremetal-кластера (%s)",
			v.NeedCores, v.NeedRAMGiB, v.MinClusterStr)
	} else {
		v.Message = fmt.Sprintf("baremetal нецелесообразен: потребность %.1f ядер / %.0f GiB загрузит минимальный кластер (%s) лишь на %.0f%% CPU / %.0f%% RAM — рекомендуем виртуальный managed K8s",
			v.NeedCores, v.NeedRAMGiB, v.MinClusterStr, v.FillCPUPct, v.FillRAMPct)
	}
	return v
}
