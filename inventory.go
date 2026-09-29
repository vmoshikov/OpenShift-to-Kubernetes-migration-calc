package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Размещение на пуле подготовленных серверов (инвентарь CSV):
// product,node_id,node_ci,node_compute_id,available,code,vendor_title,model_title,params
// params — JSON: {"cpu": ядра, "ram": GiB, "hdd": GB, "hdd_type": ..., ...};
// числа бывают и строками ("1024").
//
// Контуры делят один пул: сервер уходит ровно в один кластер. Распределение
// идёт по приоритету ПРОМ → ПСИ → IFT → DEV: прод получает лучший вариант,
// нехватка всплывает на младших контурах. Внутри контура сначала ищется
// однородный набор (один тип сервера — один node pool, проще эксплуатация),
// и только если ни одного типа не хватает — смешанный.

// InventoryServer — конкретный сервер из пула.
type InventoryServer struct {
	Product   string `json:"product"`
	NodeCI    string `json:"node_ci"`
	ComputeID string `json:"node_compute_id"`
	Available bool   `json:"available"`
	Code      string `json:"code"`
	Vendor    string `json:"vendor"`
	Model     string `json:"model"`
	Cores     int    `json:"cores"`
	RAMGiB    int    `json:"ram_gib"`
	DiskGB    int    `json:"disk_gb"`
	DiskType  string `json:"disk_type"`
}

// typeKey — серверы одного типа взаимозаменяемы для раскладки.
func (s InventoryServer) typeKey() string {
	return fmt.Sprintf("%s %dc/%dg", s.Code, s.Cores, s.RAMGiB)
}

func (s InventoryServer) config() ServerConfig {
	disks := s.DiskType
	if s.DiskGB > 0 {
		disks = fmt.Sprintf("%d GB %s", s.DiskGB, s.DiskType)
	}
	return ServerConfig{Name: s.typeKey(), Cores: s.Cores, RAMGiB: s.RAMGiB, Disks: strings.TrimSpace(disks)}
}

// ParseInventoryCSV читает CSV инвентаря. Возвращает все строки (включая
// недоступные) и предупреждения о строках, которые не удалось разобрать.
func ParseInventoryCSV(r io.Reader) ([]InventoryServer, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("CSV инвентаря: %w", err)
	}
	cols := map[string]int{"product": 0, "node_id": 1, "node_ci": 2, "node_compute_id": 3,
		"available": 4, "code": 5, "vendor_title": 6, "model_title": 7, "params": 8}
	if len(rows) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "product") {
		cols = map[string]int{}
		for i, h := range rows[0] {
			cols[strings.ToLower(strings.TrimSpace(h))] = i
		}
		rows = rows[1:]
	}
	get := func(row []string, col string) string {
		if i, ok := cols[col]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}

	var servers []InventoryServer
	var warnings []string
	for n, row := range rows {
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue
		}
		s := InventoryServer{
			Product: get(row, "product"), NodeCI: get(row, "node_ci"), ComputeID: get(row, "node_compute_id"),
			Available: strings.EqualFold(get(row, "available"), "true"),
			Code:      get(row, "code"), Vendor: get(row, "vendor_title"), Model: get(row, "model_title"),
		}
		var params map[string]interface{}
		if err := json.Unmarshal([]byte(get(row, "params")), &params); err != nil {
			warnings = append(warnings, fmt.Sprintf("строка %d (%s): params не JSON — пропущена", n+1, s.NodeCI))
			continue
		}
		s.Cores = jsonInt(params["cpu"])
		s.RAMGiB = jsonInt(params["ram"])
		s.DiskGB = jsonInt(params["hdd"])
		s.DiskType, _ = params["hdd_type"].(string)
		if s.Cores <= 0 || s.RAMGiB <= 0 {
			warnings = append(warnings, fmt.Sprintf("строка %d (%s): нет cpu/ram в params — пропущена", n+1, s.NodeCI))
			continue
		}
		servers = append(servers, s)
	}
	return servers, warnings, nil
}

// jsonInt — число из JSON, которое может прийти числом или строкой.
func jsonInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return int(f)
	}
	return 0
}

// EnvPlacement — какие конкретные серверы пула выделены контуру.
type EnvPlacement struct {
	Env      string            `json:"env"`
	Label    string            `json:"label"`
	Option   *PackingOption    `json:"option,omitempty"`
	Mixed    bool              `json:"mixed"`
	Servers  []InventoryServer `json:"servers"`
	Shortage string            `json:"shortage,omitempty"`
}

// PlacementResult — распределение пула между контурами.
type PlacementResult struct {
	Envs      []EnvPlacement `json:"envs"`
	PoolTotal int            `json:"pool_available"`
	Skipped   []string       `json:"skipped"`   // недоступные/слишком малые/битые строки
	Remaining []TypeCount    `json:"remaining"` // что осталось в пуле
}

// TypeCount — остаток пула по типу сервера.
type TypeCount struct {
	Type   string `json:"type"`
	Cores  int    `json:"cores"`
	RAMGiB int    `json:"ram_gib"`
	Count  int    `json:"count"`
}

// PlaceOnInventory распределяет доступные серверы пула между контурами.
func PlaceOnInventory(results []EnvSizingResult, inventory []InventoryServer, parseWarnings []string, p PackingParams) PlacementResult {
	out := PlacementResult{Skipped: append([]string{}, parseWarnings...)}

	// Пул: только доступные и не меньше минимального барика.
	pool := map[string][]InventoryServer{}
	for _, s := range inventory {
		switch {
		case !s.Available:
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s (%s): available=false", s.NodeCI, s.Code))
		case s.Cores < p.MinServer.Cores || s.RAMGiB < p.MinServer.RAMGiB:
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s (%s, %d/%d): меньше минимального барика %d/%d",
				s.NodeCI, s.Code, s.Cores, s.RAMGiB, p.MinServer.Cores, p.MinServer.RAMGiB))
		default:
			pool[s.typeKey()] = append(pool[s.typeKey()], s)
			out.PoolTotal++
		}
	}

	// Приоритет контуров: HA и крупные — первыми (ПРОМ → ПСИ → IFT → DEV).
	order := append([]EnvSizingResult(nil), results...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].Env.ReplicaFactor > order[j].Env.ReplicaFactor })

	// Основной тип пула — самый многочисленный: по нему оцениваем, сколько докупить.
	var std InventoryServer
	stdN := 0
	for key, list := range pool {
		if len(list) > stdN || (len(list) == stdN && key < std.typeKey()) {
			std, stdN = list[0], len(list)
		}
	}
	for _, r := range order {
		out.Envs = append(out.Envs, placeEnv(r, pool, std, p))
	}
	// Вернуть исходный порядок контуров для вывода.
	pos := map[string]int{}
	for i, r := range results {
		pos[r.Env.Key] = i
	}
	sort.SliceStable(out.Envs, func(i, j int) bool { return pos[out.Envs[i].Env] < pos[out.Envs[j].Env] })

	for key, list := range pool {
		if len(list) > 0 {
			out.Remaining = append(out.Remaining, TypeCount{Type: key, Cores: list[0].Cores, RAMGiB: list[0].RAMGiB, Count: len(list)})
		}
	}
	sort.Slice(out.Remaining, func(i, j int) bool { return out.Remaining[i].Type < out.Remaining[j].Type })
	return out
}

func placeEnv(r EnvSizingResult, pool map[string][]InventoryServer, std InventoryServer, p PackingParams) EnvPlacement {
	ep := EnvPlacement{Env: r.Env.Key, Label: r.Env.Label, Servers: []InventoryServer{}}
	pods, replicas := podsOf(r.Deployments)
	ha := r.Env.MinReplicas >= 2

	// 1. Однородный набор: тип, которого в пуле хватает, с наименьшим объёмом железа.
	var bestKey string
	var best *PackingOption
	for key, list := range pool {
		if len(list) == 0 {
			continue
		}
		opt, _ := packServers(pods, replicas, unlimitedSupply{list[0].config()}, list[0].config(), p, ha, ha)
		if !opt.Feasible || opt.Nodes > len(list) {
			continue
		}
		opt.Server = list[0].config()
		if best == nil || betterPacking(opt, *best) || (!betterPacking(*best, opt) && key < bestKey) {
			o := opt
			best, bestKey = &o, key
		}
	}
	if best != nil {
		ep.Option = best
		ep.Servers = append(ep.Servers, pool[bestKey][:best.Nodes]...)
		pool[bestKey] = pool[bestKey][best.Nodes:]
		return ep
	}

	// 2. Смешанный набор: конкретные серверы пула от крупных к мелким —
	// крупные закрывают крупные поды. Запасной N+1 резервируется заранее и
	// это самый крупный сервер: он примет нагрузку любой упавшей ноды.
	all := poolSorted(pool)
	if len(all) == 0 {
		ep.Shortage = shortage(pods, replicas, all, std, p, ha)
		return ep
	}
	var spare *InventoryServer
	rest := all
	if ha {
		spare, rest = &all[0], all[1:]
	}
	configs := make([]ServerConfig, len(rest))
	for i, s := range rest {
		configs[i] = s.config()
	}
	ref := all[0].config()
	opt, used := packServers(pods, replicas, &listSupply{servers: configs}, ref, p, ha, false)
	if !opt.Feasible || (ha && spare == nil) {
		ep.Shortage = shortage(pods, replicas, all, std, p, ha)
		return ep
	}
	chosen := append([]InventoryServer{}, rest[:len(used)]...)
	if spare != nil {
		chosen = append(chosen, *spare)
		used = append(used, spare.config())
		opt.SpareNodes = 1
		opt.Nodes++
		opt.TotalCores += spare.Cores
		opt.TotalRAMGiB += spare.RAMGiB
		opt.Composition = composition(used)
	}
	opt.Server = ServerConfig{Name: "смешанный набор"}
	ep.Option, ep.Mixed, ep.Servers = &opt, true, chosen
	takeFromPool(pool, chosen)
	return ep
}

// poolSorted — все оставшиеся серверы пула от крупных к мелким.
func poolSorted(pool map[string][]InventoryServer) []InventoryServer {
	var all []InventoryServer
	for _, list := range pool {
		all = append(all, list...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Cores != all[j].Cores {
			return all[i].Cores > all[j].Cores
		}
		if all[i].RAMGiB != all[j].RAMGiB {
			return all[i].RAMGiB > all[j].RAMGiB
		}
		return all[i].NodeCI < all[j].NodeCI
	})
	return all
}

func takeFromPool(pool map[string][]InventoryServer, chosen []InventoryServer) {
	take := map[string]bool{}
	for _, s := range chosen {
		take[s.NodeCI+"|"+s.ComputeID] = true
	}
	for key, list := range pool {
		var rest []InventoryServer
		for _, s := range list {
			if !take[s.NodeCI+"|"+s.ComputeID] {
				rest = append(rest, s)
			}
		}
		pool[key] = rest
	}
}

// shortage оценивает, сколько серверов докупить: остаток пула плюс сколько
// угодно серверов основного типа пула.
func shortage(pods []packPod, replicas map[string]int, all []InventoryServer, std InventoryServer, p PackingParams, ha bool) string {
	if std.Cores == 0 {
		return "в пуле нет подходящих серверов"
	}
	configs := make([]ServerConfig, len(all))
	for i, s := range all {
		configs[i] = s.config()
	}
	ref := std.config()
	if len(all) > 0 {
		ref = configs[0]
	}
	sup := &extendedSupply{list: listSupply{servers: configs}, extra: std.config()}
	opt, used := packServers(pods, replicas, sup, ref, p, ha, ha)
	if !opt.Feasible {
		return fmt.Sprintf("в пуле не хватает серверов (осталось %d); %s", len(all), opt.Reason)
	}
	return fmt.Sprintf("в пуле не хватает серверов: осталось %d, нужно ещё ≈%d × %s (%s, %d ядер / %d GiB)",
		len(all), len(used)-len(all), std.Code, std.Model, std.Cores, std.RAMGiB)
}

// extendedSupply — сначала конкретные серверы, затем сколько угодно extra.
type extendedSupply struct {
	list  listSupply
	extra ServerConfig
}

func (e *extendedSupply) next() (ServerConfig, bool) {
	if s, ok := e.list.next(); ok {
		return s, true
	}
	return e.extra, true
}

// podsOf — поды контура из поштучного плана (общая часть с PackEnv).
func podsOf(plans []DeploymentPlan) ([]packPod, map[string]int) {
	var pods []packPod
	replicas := map[string]int{}
	for _, d := range plans {
		var pp packPod
		pp.app = d.Namespace + "/" + d.Name
		for _, c := range d.Containers {
			pp.reqCPU += c.Requests.CPUMilli
			pp.reqMem += c.Requests.MemMiB
			pp.limCPU += c.Limits.CPUMilli
			pp.limMem += c.Limits.MemMiB
		}
		replicas[pp.app] = d.Replicas
		for i := 0; i < d.Replicas; i++ {
			pods = append(pods, pp)
		}
	}
	return pods, replicas
}

// WritePlacementCSV — выгрузка назначения: какие серверы в какой кластер.
func WritePlacementCSV(w io.Writer, pr PlacementResult) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"env", "node_ci", "node_compute_id", "code", "vendor_title", "model_title", "cores", "ram_gib"})
	for _, e := range pr.Envs {
		for _, s := range e.Servers {
			_ = cw.Write([]string{e.Env, s.NodeCI, s.ComputeID, s.Code, s.Vendor, s.Model, strconv.Itoa(s.Cores), strconv.Itoa(s.RAMGiB)})
		}
	}
	cw.Flush()
	return cw.Error()
}
