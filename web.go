package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

// Минимальный веб-UI поверх тех же функций, что и CLI (CalculateEnvSizing,
// DeploymentsFromPastedJSON, DeploymentsFromLLMExtraction) — форма для
// ручного ввода ресурсов стенда прямо в окне браузера, без консоли.
// Один статический HTML, без JS/CSS-фреймворков — чтобы не тянуть
// зависимости в демо, которое должно собираться и работать офлайн.

var uiTemplate = template.Must(template.New("ui").Funcs(template.FuncMap{
	"millicores": func(m int64) float64 { return float64(m) / 1000 },
	"gib":        func(m int64) float64 { return float64(m) / 1024 },
	"change":     fmtChange,
	"md":         renderMarkdown,
	"changei":    func(b, a int) string { return fmtChange(float64(b), float64(a), 0) },
}).Parse(`<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<title>mk8s-calc</title>
<style>
 body{font-family:system-ui,sans-serif;max-width:920px;margin:2rem auto;padding:0 1rem;color:#1a1a1a}
 h1{font-size:1.3rem}
 h2{font-size:1.05rem;margin-top:2rem}
 h3{font-size:.95rem;margin-top:1.4rem}
 tr.best td{background:#f0fdf4;font-weight:600}
 details{margin-top:.4rem}
 .md{background:#f9fafb;border:1px solid #e5e7eb;border-radius:6px;padding:.4rem 1rem;font-size:.9rem;line-height:1.5}
 .md h3,.md h4,.md h5,.md h6{margin:1rem 0 .4rem}
 .md p{margin:.5rem 0}
 .md ul,.md ol{margin:.4rem 0;padding-left:1.4rem}
 .md li{margin:.15rem 0}
 .md code{background:#eef0f3;padding:.05rem .3rem;border-radius:4px;font-size:.85em}
 .md pre{background:#1f2937;color:#f9fafb;padding:.7rem;border-radius:6px;overflow-x:auto}
 .md pre code{background:none;padding:0;color:inherit}
 .md blockquote{margin:.5rem 0;padding:.1rem .8rem;border-left:3px solid #cbd5e1;color:#475569}
 .md table{margin:.5rem 0}
 .md hr{border:none;border-top:1px solid #e5e7eb;margin:.8rem 0}
 fieldset{border:1px solid #ccc;border-radius:6px;margin-bottom:1rem;padding:1rem}
 label{display:block;margin:.6rem 0 .2rem;font-weight:600;font-size:.9rem}
 select,textarea{width:100%;box-sizing:border-box;font-family:inherit;padding:.4rem;font-size:.9rem}
 textarea{min-height:160px;font-family:ui-monospace,monospace;font-size:.82rem}
 .checkbox{display:flex;align-items:center;gap:.4rem;font-weight:normal;margin-top:.8rem}
 .checkbox input{width:auto}
 button{padding:.5rem 1.2rem;background:#1a56db;color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:.95rem}
 table{border-collapse:collapse;width:100%;margin-top:.5rem}
 th,td{border:1px solid #ddd;padding:.4rem .6rem;text-align:left;font-size:.85rem}
 th{background:#f3f4f6}
 .err{color:#b91c1c;white-space:pre-wrap;background:#fef2f2;padding:.6rem;border-radius:6px;font-size:.85rem}
 .total{font-weight:700;margin-top:.6rem}
 .hint{color:#6b7280;font-size:.8rem;margin:.2rem 0 0}
 table.ovr input[type=number]{width:5.5rem;box-sizing:border-box;padding:.2rem;font-size:.85rem}
 .warn{background:#fffbeb;border:1px solid #fcd34d;color:#92400e;padding:.5rem .7rem;border-radius:6px;font-size:.85rem}
 .ok{background:#f0fdf4;border:1px solid #86efac;color:#166534;padding:.5rem .7rem;border-radius:6px;font-size:.85rem}
 .star{color:#b45309;font-weight:600;white-space:nowrap}
 .was{color:#6b7280;font-size:.75rem}
 pre{white-space:pre-wrap;background:#f9fafb;padding:.8rem;border-radius:6px;font-size:.85rem}
</style>
</head>
<body>
<h1>mk8s-calc — sizing OpenShift → managed Kubernetes</h1>
<form method="post" action="/">
 <fieldset>
  <label>Источник данных</label>
  <select name="source">
   <option value="mock" {{if eq .Source "mock"}}selected{{end}}>mock — синтетический стенд (демо)</option>
   <option value="paste" {{if eq .Source "paste"}}selected{{end}}>paste — вставить JSON (oc/kubectl get deployments -o json)</option>
   <option value="llm-paste" {{if eq .Source "llm-paste"}}selected{{end}}>llm-paste — вставить произвольный текст (распознаёт LLM)</option>
  </select>
  <p class="hint">Для paste/llm-paste заполните поле ниже. Для mock поле игнорируется.</p>

  <label>Ручной ввод</label>
  <textarea name="paste" placeholder="paste: вставьте JSON-дамп деплойментов&#10;llm-paste: вставьте что угодно похожее на выгрузку ресурсов (oc get all, oc describe dc, таблицу requests/limits)">{{.PasteInput}}</textarea>

  <label>Пул серверов для размещения (CSV, необязательно)</label>
  <textarea name="inventory" style="min-height:90px" placeholder="product,node_id,node_ci,node_compute_id,available,code,vendor_title,model_title,params&#10;MANAGED_K8S,0,CI02690156,a09lclq2,true,X86_R_2S_SDS_GENERAL_32,Huawei,Huawei 2288H V5,&quot;{&quot;&quot;cpu&quot;&quot;: 32, &quot;&quot;ram&quot;&quot;: 768}&quot;">{{.InventoryInput}}</textarea>
  <p class="hint">Если задан — серверы для кластеров выбираются из этого пула: только available=true и не меньше минимального барика; контуры делят пул по приоритету ПРОМ → ПСИ → IFT → DEV.</p>

  <label>Данные сняты со стенда</label>
  <select name="from">
   {{range .Profiles}}<option value="{{.Key}}" {{if eq .Key $.From}}selected{{end}}>{{.Label}}</option>
   {{end}}
  </select>
  <p class="hint">Реплики целевых контуров пересчитываются от этого стенда.</p>

  <label>Контур</label>
  <select name="env">
   <option value="all" {{if eq .Env "all"}}selected{{end}}>все контуры</option>
   <option value="dev" {{if eq .Env "dev"}}selected{{end}}>DEV</option>
   <option value="ift" {{if eq .Env "ift"}}selected{{end}}>IFT (тест)</option>
   <option value="psi" {{if eq .Env "psi"}}selected{{end}}>ПСИ (приёмка)</option>
   <option value="prom" {{if eq .Env "prom"}}selected{{end}}>ПРОМ</option>
  </select>

  <label>Override — сжатие завышенных limits, по контурам</label>
  <table class="ovr">
  <tr><th>Контур</th><th>Пропустить</th><th>Интенсивность</th></tr>
  {{range .Profiles}}
  <tr>
   <td>{{.Label}}</td>
   <td><input type="checkbox" name="skip_{{.Key}}" value="1" {{if .Skip}}checked{{end}}></td>
   <td><select name="ovr_{{.Key}}">{{$lvl := .Override.Level}}{{range $.Levels}}<option value="{{.}}" {{if eq . $lvl}}selected{{end}}>{{.}}</option>{{end}}</select></td>
  </tr>
  {{end}}
  </table>
  <p class="hint">Если limit контейнера во много раз больше request, он «съедает» ёмкость в сумме Limits. Override уменьшает такие limits.
  <b>off</b> — не трогать; <b>soft</b> — только limit &gt; request ×3, сжать до ×2; <b>medium</b> — &gt; ×2 → ×1.5; <b>hard</b> — &gt; ×1.5 → ×1.2;
  <b>auto</b> — калькулятор сам переберёт уровни и возьмёт самый мягкий, который реально уменьшает кластер (рекомендуется);
  <b>custom</b> — свои числа из «Тонкой настройки».</p>

  <details>
  <summary class="hint">Тонкая настройка (нужна только для custom; floor действует всегда)</summary>
  <p class="hint">Правило для каждого контейнера: если <b>limit / request &gt; порог</b>, то новый limit = <b>max(request, floor) × цель</b>
  (только если он меньше старого и не ниже request).<br>
  Пример: request 100m, limit 2000m, порог 2, цель 1.5, floor 250m → 2000/100 = 20 &gt; 2 → max(100, 250) × 1.5 = <b>375m</b>.<br>
  <b>Порог</b> — с какого расхождения limit/request считать limit завышенным. <b>Цель</b> — во сколько раз новый limit больше request.
  <b>Floor</b> — минимальная база: крошечные requests (напр. 10m) не превратятся в нереально малый limit.
  Для soft/medium/hard/auto порог и цель берутся из пресета — эти поля игнорируются.</p>
  <table class="ovr">
  <tr><th>Контур</th><th>CPU порог</th><th>CPU цель</th><th>CPU floor, m</th><th>Mem порог</th><th>Mem цель</th><th>Mem floor, MiB</th></tr>
  {{range .Profiles}}
  <tr>
   <td>{{.Label}}</td>
   <td><input type="number" step="0.1" min="1" name="ovr_cpu_ratio_{{.Key}}" value="{{.Override.CPU.Ratio}}"></td>
   <td><input type="number" step="0.1" min="1" name="ovr_cpu_target_{{.Key}}" value="{{.Override.CPU.Target}}"></td>
   <td><input type="number" step="50" min="0" name="ovr_cpu_floor_{{.Key}}" value="{{.Override.CPU.Floor}}"></td>
   <td><input type="number" step="0.1" min="1" name="ovr_mem_ratio_{{.Key}}" value="{{.Override.Mem.Ratio}}"></td>
   <td><input type="number" step="0.1" min="1" name="ovr_mem_target_{{.Key}}" value="{{.Override.Mem.Target}}"></td>
   <td><input type="number" step="64" min="0" name="ovr_mem_floor_{{.Key}}" value="{{.Override.Mem.Floor}}"></td>
  </tr>
  {{end}}
  </table>
  </details>
  <p class="hint">«Пропустить» исключает контур из расчёта.</p>

  <label>Baremetal: профиль приложения</label>
  <select name="profile">
   {{range .AppProfiles}}<option value="{{.Key}}" {{if eq .Key $.Profile}}selected{{end}}>{{.Label}} — {{.Note}}</option>
   {{end}}
  </select>
  <p class="hint">Профиль задаёт, насколько плотно укладывать поды: потолок утилизации по requests и допустимое уплотнение по limits (OLTP мягче, DWH жёстче).
  Лимит подов на ноду (kubelet maxPods): <input type="number" name="max_pods" min="10" max="1000" value="{{.MaxPods}}" style="width:5rem"></p>

  <label class="checkbox"><input type="checkbox" name="review" value="1" {{if .Review}}checked{{end}}> ревью плана LLM (нужен OPENAI_API_KEY на сервере)</label>

  <p><button type="submit">Посчитать</button> <button type="submit" name="format" value="json">Скачать план JSON</button> <button type="submit" name="format" value="zip">Скачать манифесты (zip)</button> <button type="submit" name="format" value="placement">Скачать назначение серверов (CSV)</button></p>
  <p class="hint">JSON — тот же контракт, что <code>-json</code> в CLI: POST с <code>format=json</code> работает как HTTP-API для агента.</p>
 </fieldset>
</form>

{{if .Error}}<div class="err">{{.Error}}</div>{{end}}

{{if .Results}}
<h2>Результат</h2>
<p>Деплойментов: {{.DeploymentCount}} · Подов на исходном стенде: {{.TotalPods}}</p>
<p class="hint">Базис — limits из манифестов. Фактическое потребление (Prometheus p95/p99) не подключено: override сжимает limits по формуле, а не по реальной нагрузке — перед применением сверить с метриками.</p>
{{if .DataWarnings}}<h3>Предупреждения по данным</h3><ul>{{range .DataWarnings}}<li class="hint">{{.String}}</li>{{end}}</ul>{{end}}
<h3>Методика (сумма Limits × коэффициент Таблицы 2)</h3>
<table>
<tr><th>Контур</th><th>Подов</th><th>V, ядер</th><th>RAM спрос</th><th>Коэфф.</th><th>Диапазон V</th><th>Размер кластера</th><th>Нода</th><th>Нод</th><th>Ёмкость</th><th>Плотность CPU / RAM</th></tr>
{{range .Results}}
<tr>
 <td>{{.Env.Label}}{{if .Env.Override.Enabled}} <span class="star" title="{{.Env.Override.Level}}, сжато limits: {{.OverriddenLimits}}">★override</span>{{end}}{{with .Recommendation}}<br><span class="was" title="{{.Reason}}">auto → {{.Level}}: {{.Reason}}</span>{{end}}</td>
 <td>{{.TotalPods}}</td>
 <td>{{printf "%.2f" .Table.UserCores}}{{if ne .LimitCPUMilli .OrigLimitCPUMilli}}<br><span class="was">было {{printf "%.2f" (millicores .OrigLimitCPUMilli)}}</span>{{end}}</td>
 <td>{{printf "%.1f" .Table.UserRAMGiB}} GiB{{if ne .LimitMemMiB .OrigLimitMemMiB}}<br><span class="was">было {{printf "%.1f" (gib .OrigLimitMemMiB)}} GiB</span>{{end}}</td>
 <td>{{printf "%.2f" .Table.Best.Coef}}</td>
 <td>{{.Table.Best.CoefRange}}</td>
 <td>{{printf "%.1f" .Table.Best.ClusterCores}} ядер</td>
 <td>{{.Table.Best.Name}}</td>
 <td>{{.Table.Best.Nodes}}</td>
 <td>{{.Table.Best.NominalCores}} ядер / {{.Table.Best.NominalRAMGiB}} GiB{{if .Table.Best.RAMBound}}<br><span class="was">ноды добавлены под RAM</span>{{end}}</td>
 <td>{{printf "%.0f" .Table.Best.CPUUtil}}% / {{printf "%.0f" .Table.Best.RAMUtil}}%</td>
</tr>
{{end}}
</table>
<details><summary class="hint">Все варианты перебора</summary>
<table>
<tr><th>Контур</th><th>Нода</th><th>Коэфф.</th><th>Размер кластера</th><th>Нод</th><th>Ёмкость</th><th>Плотность CPU / RAM</th></tr>
{{range .Results}}{{$best := .Table.Best}}{{$label := .Env.Label}}{{range .Table.Variants}}
<tr{{if eq . $best}} class="best"{{end}}>
 <td>{{$label}}</td>
 <td>{{.Name}}{{if eq . $best}} ✓{{end}}</td>
 <td>{{printf "%.2f" .Coef}}</td>
 <td>{{printf "%.1f" .ClusterCores}} ядер</td>
 <td>{{.Nodes}}{{if .RAMBound}} (под RAM){{end}}</td>
 <td>{{.NominalCores}} ядер / {{.NominalRAMGiB}} GiB</td>
 <td>{{printf "%.0f" .CPUUtil}}% / {{printf "%.0f" .RAMUtil}}%</td>
</tr>
{{end}}{{end}}
</table>
</details>

<h3>Подбор baremetal (раскладка подов по серверам)</h3>
{{with .BaremetalSummary}}<div class="{{if .Worthwhile}}ok{{else}}warn{{end}}">{{.Text}}</div>{{end}}
<p class="hint">Для каждой конфигурации сервера поды раскладываются по нодам с учётом requests, уплотнения по limits, лимита подов на ноду,
системного резерва и DaemonSet, разнесения реплик и N+1 на ПСИ/ПРОМ. Выбран вариант с наименьшим объёмом железа.</p>
<table>
<tr><th>Контур</th><th>Подов</th><th>Серверы</th><th>Всего</th><th>Уплотнение CPU / RAM</th><th>Req CPU / RAM</th><th>Подов на ноду</th><th>Узкое место</th></tr>
{{range .Results}}{{with .Packing}}{{$p := .}}
<tr>
 <td>{{$.EnvLabel $p}}</td>
 <td>{{.Pods}}<br><span class="was">CPU p50 {{.PodStats.CPUP50Milli}}m / max {{.PodStats.CPUMaxMilli}}m</span></td>
 {{with .Best}}
 <td><b>{{.Nodes}} × {{.Server.Name}}</b>{{if .SpareNodes}}<br><span class="was">{{.NodesPacked}} + {{.SpareNodes}} N+1</span>{{end}}<br><span class="was">{{.Server.Cores}} ядер / {{.Server.RAMGiB}} GiB, {{.Server.Disks}}</span></td>
 <td>{{.TotalCores}} ядер / {{.TotalRAMGiB}} GiB</td>
 <td>×{{printf "%.2f" .CPULimitK}} / ×{{printf "%.2f" .MemLimitK}}</td>
 <td>{{printf "%.0f" .CPUReqUtil}}% / {{printf "%.0f" .MemReqUtil}}%</td>
 <td>{{printf "%.0f" .PodsPerNode}} (max {{.MaxPodsOnNode}})</td>
 <td>{{.Bottleneck}}</td>
 {{else}}<td colspan="6" class="err">ни одна конфигурация не подходит</td>{{end}}
</tr>
{{if not .Verdict.Worthwhile}}<tr><td colspan="8" class="warn">⚠ {{.Verdict.Message}}</td></tr>{{end}}
{{range .Warnings}}<tr><td colspan="8" class="err">{{.}}</td></tr>{{end}}
{{end}}{{end}}
</table>
<details><summary class="hint">Все конфигурации серверов по контурам</summary>
<table>
<tr><th>Контур</th><th>Сервер</th><th>Серверов</th><th>Всего</th><th>Уплотнение CPU / RAM</th><th>Req CPU / RAM</th><th>Подов на ноду</th><th>Узкое место</th></tr>
{{range .Results}}{{$label := .Env.Label}}{{with .Packing}}{{$best := .Best}}{{range .Options}}
<tr{{if and $best (eq .Server.Name $best.Server.Name)}} class="best"{{end}}>
 <td>{{$label}}</td><td>{{.Server.Name}}</td>
 {{if .Feasible}}
 <td>{{.NodesPacked}}{{if .SpareNodes}} + {{.SpareNodes}}{{end}}</td>
 <td>{{.TotalCores}} / {{.TotalRAMGiB}} GiB</td>
 <td>×{{printf "%.2f" .CPULimitK}} / ×{{printf "%.2f" .MemLimitK}}</td>
 <td>{{printf "%.0f" .CPUReqUtil}}% / {{printf "%.0f" .MemReqUtil}}%</td>
 <td>{{printf "%.0f" .PodsPerNode}} (max {{.MaxPodsOnNode}})</td>
 <td>{{.Bottleneck}}</td>
 {{else}}<td colspan="6">не подходит: {{.Reason}}</td>{{end}}
</tr>
{{end}}{{end}}{{end}}
</table>
</details>

{{with .Placement}}
<h3>Размещение на пуле серверов (доступно {{.PoolTotal}})</h3>
<table>
<tr><th>Контур</th><th>Серверы</th><th>Всего</th><th>Уплотнение CPU</th><th>Узкое место</th><th>Назначенные серверы (node_ci / compute_id)</th></tr>
{{range .Envs}}
<tr>
 <td>{{.Label}}</td>
 {{if .Option}}
 <td>{{.Option.Nodes}} {{if .Mixed}}(смешанный){{else}}(однородный){{end}}{{if .Option.SpareNodes}}<br><span class="was">из них {{.Option.SpareNodes}} N+1</span>{{end}}
  {{range .Option.Composition}}<br><span class="was">{{.Count}} × {{.Server.Name}}</span>{{end}}</td>
 <td>{{.Option.TotalCores}} ядер / {{.Option.TotalRAMGiB}} GiB</td>
 <td>×{{printf "%.2f" .Option.CPULimitK}}</td>
 <td>{{.Option.Bottleneck}}</td>
 <td class="was">{{range .Servers}}{{.NodeCI}} / {{.ComputeID}} — {{.Model}} ({{.Cores}}/{{.RAMGiB}})<br>{{end}}</td>
 {{else}}
 <td colspan="5" class="warn">⚠ {{.Shortage}}</td>
 {{end}}
</tr>
{{end}}
</table>
{{if .Remaining}}<p class="hint">Осталось в пуле: {{range .Remaining}}{{.Count}} × {{.Type}}; {{end}}</p>{{end}}
{{if .Skipped}}<details><summary class="hint">Не вошли в пул ({{len .Skipped}})</summary><ul>{{range .Skipped}}<li class="hint">{{.}}</li>{{end}}</ul></details>{{end}}
{{end}}

{{if .Effects}}
<h3>Эффект override (было → стало)</h3>
<table>
<tr><th>Контур</th><th>Уровень</th><th>Сжато limits</th><th>V, ядер</th><th>RAM спрос, GiB</th><th>Размер кластера, ядер</th><th>Нод</th><th>Ёмкость, ядер</th><th>Ёмкость RAM, GiB</th></tr>
{{range .Effects}}
<tr{{if eq .Label "Итого"}} class="best"{{end}}>
 <td>{{.Label}}{{if .Auto}} <span class="was">auto</span>{{end}}{{if .Worse}} <span class="star" title="V ушло в диапазон Таблицы 2 с большим коэффициентом — ослабьте уровень">⚠ кластер вырос</span>{{end}}</td>
 <td>{{.Level}}</td>
 <td>{{.Compressed}}</td>
 <td>{{change .CoresBefore .CoresAfter 2}}</td>
 <td>{{change .RAMBefore .RAMAfter 1}}</td>
 <td>{{change .SizeBefore .SizeAfter 1}}</td>
 <td>{{changei .NodesBefore .NodesAfter}}</td>
 <td>{{changei .NomCPUBefore .NomCPUAfter}}</td>
 <td>{{changei .NomRAMBefore .NomRAMAfter}}</td>
</tr>
{{end}}
</table>
{{end}}
<p class="hint">Перебор {8, 16 vCPU} × {RAM ×2, ×4}: минимум нод → покрытие RAM → наименьший номинальный RAM. Нод не менее 2. Limits не заданы → берутся requests.</p>

{{end}}

{{if .ReviewText}}
<h2>Ревью модели</h2>
<div class="md">{{md .ReviewText}}</div>
{{end}}

</body>
</html>
`))

type uiPageData struct {
	Source          string
	Env             string
	PasteInput      string
	Review          bool
	From            string
	Profile         string // профиль приложения для подбора baremetal
	InventoryInput  string
	Placement       *PlacementResult
	MaxPods         int
	AppProfiles     []AppProfile
	Profiles        []EnvProfile
	Levels          []string
	Error           string
	Results         []EnvSizingResult
	DeploymentCount int
	TotalPods       int
	Effects         []OverrideEffect // по контурам с override + «Итого»
	DataWarnings    []DataWarning
	ReviewText      string
}

// RunWebUI поднимает http-сервер с формой sizing на addr (напр. "127.0.0.1:8080")
// и блокирует до его остановки/ошибки.
func RunWebUI(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleUIIndex)

	fmt.Printf("UI слушает на http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

func handleUIIndex(w http.ResponseWriter, r *http.Request) {
	data := uiPageData{Source: "mock", Env: "all", From: "prom", Profiles: DefaultProfiles(), Levels: OverrideLevels,
		Profile: "mixed", MaxPods: DefaultPackingParams().MaxPodsPerNode, AppProfiles: AppProfiles}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			data.Error = "не удалось разобрать форму: " + err.Error()
			renderUI(w, data)
			return
		}
		data.Source = r.FormValue("source")
		data.Env = r.FormValue("env")
		data.PasteInput = r.FormValue("paste")
		data.InventoryInput = r.FormValue("inventory")
		data.Review = r.FormValue("review") == "1"
		if f := r.FormValue("from"); f != "" {
			data.From = f
		}
		if v := r.FormValue("profile"); v != "" {
			data.Profile = v
		}
		if v := r.FormValue("max_pods"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 10 {
				data.Error = "лимит подов на ноду должен быть целым числом ≥ 10"
				renderUI(w, data)
				return
			}
			data.MaxPods = n
		}
		pk := DefaultPackingParams()
		pk.MaxPodsPerNode = data.MaxPods
		var perr error
		if pk.Profile, perr = ProfileByKeyApp(data.Profile); perr != nil {
			data.Error = perr.Error()
			renderUI(w, data)
			return
		}
		if err := readProfileSettings(r, data.Profiles); err != nil {
			data.Error = err.Error()
			renderUI(w, data)
			return
		}

		deployments, err := loadDeploymentsForUI(data.Source, data.PasteInput)
		if err != nil {
			data.Error = err.Error()
			renderUI(w, data)
			return
		}

		data.DeploymentCount = len(deployments)
		data.DataWarnings = AnalyzeDeployments(deployments)
		for _, d := range deployments {
			data.TotalPods += d.Replicas
		}

		fromEnv, err := ProfileByKey(data.Profiles, data.From)
		if err != nil {
			data.Error = err.Error()
			renderUI(w, data)
			return
		}

		for i := range data.Profiles {
			env := data.Profiles[i]
			if (data.Env != "all" && data.Env != env.Key) || env.Skip {
				continue
			}
			// Пресет перезаписывает порог и цель из полей; SetLevel проверяет значения.
			rec, err := ApplyOverrideLevel(deployments, fromEnv, &env, env.Override.Level)
			if err != nil {
				data.Error = env.Label + ": " + err.Error()
				renderUI(w, data)
				return
			}
			if rec == nil {
				data.Profiles[i].Override = env.Override // показать применённые значения пресета
			}
			res := CalculateEnvSizing(deployments, fromEnv, env)
			res.Recommendation = rec
			packing := PackEnv(res.Deployments, env, pk)
			res.Packing = &packing
			data.Results = append(data.Results, res)
		}
		if len(data.Results) == 0 {
			data.Error = "нет контуров для расчёта: выбранный контур пропущен или все контуры отмечены «Пропустить»"
		}

		var total OverrideEffect
		for _, res := range data.Results {
			if res.Env.Override.Enabled() {
				e := res.Effect()
				data.Effects = append(data.Effects, e)
				total.Add(e)
			}
		}
		if len(data.Effects) > 1 {
			total.Label, total.Level = "Итого", "—"
			data.Effects = append(data.Effects, total)
		}

		if strings.TrimSpace(data.InventoryInput) != "" && data.Error == "" {
			inv, warns, err := ParseInventoryCSV(strings.NewReader(data.InventoryInput))
			if err != nil {
				data.Error = err.Error()
				renderUI(w, data)
				return
			}
			pl := PlaceOnInventory(data.Results, inv, warns, pk)
			data.Placement = &pl
		}

		if r.FormValue("format") == "placement" && data.Error == "" {
			if data.Placement == nil {
				data.Error = "для выгрузки назначения вставьте CSV пула серверов"
				renderUI(w, data)
				return
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="mk8s-placement.csv"`)
			if err := WritePlacementCSV(w, *data.Placement); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		if r.FormValue("format") == "zip" && data.Error == "" {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="mk8s-manifests.zip"`)
			if _, err := WriteManifestsZip(w, data.Results); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		if r.FormValue("format") == "json" && data.Error == "" {
			rep := BuildPlanReport(data.Source, "all", fromEnv, deployments, data.Results)
			rep.Packing = &pk
			rep.Placement = data.Placement
			if data.Review {
				if text, err := ReviewSizingPlan(LoadReviewConfigFromEnv(), data.Results); err != nil {
					rep.ReviewError = err.Error()
				} else {
					rep.Review = text
				}
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="mk8s-plan.json"`)
			if err := rep.WriteJSON(w); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		if data.Review {
			cfg := LoadReviewConfigFromEnv()
			text, err := ReviewSizingPlan(cfg, data.Results)
			if err != nil {
				data.Error = "Ревью недоступно: " + err.Error()
			} else {
				data.ReviewText = text
			}
		}
	}

	renderUI(w, data)
}

// readProfileSettings заполняет per-контурные настройки (пропуск, override)
// из полей формы skip_<key>, ovr_<key>, ovr_{cpu,mem}_{ratio,target,floor}_<key>.
func readProfileSettings(r *http.Request, profiles []EnvProfile) error {
	for i := range profiles {
		p := &profiles[i]
		p.Skip = r.FormValue("skip_"+p.Key) == "1"
		rules := []struct {
			res  string
			rule *OverrideRule
		}{{"cpu", &p.Override.CPU}, {"mem", &p.Override.Mem}}
		for _, x := range rules {
			var err error
			if x.rule.Ratio, err = formFloat(r, "ovr_"+x.res+"_ratio_"+p.Key, x.rule.Ratio); err != nil {
				return fmt.Errorf("%s: %w", p.Label, err)
			}
			if x.rule.Target, err = formFloat(r, "ovr_"+x.res+"_target_"+p.Key, x.rule.Target); err != nil {
				return fmt.Errorf("%s: %w", p.Label, err)
			}
			floor, err := formFloat(r, "ovr_"+x.res+"_floor_"+p.Key, float64(x.rule.Floor))
			if err != nil {
				return fmt.Errorf("%s: %w", p.Label, err)
			}
			x.rule.Floor = int64(floor)
		}
		// Уровень применяется позже (ApplyOverrideLevel): для auto нужны данные.
		p.Override.Level = r.FormValue("ovr_" + p.Key)
	}
	return nil
}

// formFloat читает неотрицательное число из поля формы; пустое поле — def.
func formFloat(r *http.Request, name string, def float64) (float64, error) {
	v := r.FormValue(name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def, fmt.Errorf("некорректное значение поля %s: %q", name, v)
	}
	return f, nil
}

func renderUI(w http.ResponseWriter, data uiPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uiTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func loadDeploymentsForUI(source, pasteInput string) ([]DeploymentSpec, error) {
	switch source {
	case "mock":
		return MockDeployments(), nil
	case "paste":
		if pasteInput == "" {
			return nil, fmt.Errorf("вставьте JSON в поле ручного ввода")
		}
		return DeploymentsFromPastedJSON(pasteInput)
	case "llm-paste":
		if pasteInput == "" {
			return nil, fmt.Errorf("вставьте текст выгрузки в поле ручного ввода")
		}
		return DeploymentsFromLLMExtraction(LoadReviewConfigFromEnv(), pasteInput)
	default:
		return nil, fmt.Errorf("неизвестный источник %q", source)
	}
}

// EnvLabel — подпись контура для результата раскладки (шаблону нужен
// переход от PackingResult обратно к контуру).
func (d uiPageData) EnvLabel(p *PackingResult) string {
	for _, r := range d.Results {
		if r.Packing == p {
			return r.Env.Label
		}
	}
	return ""
}

// BaremetalSummary — итоговый вердикт по baremetal: то, что клиенту стоит
// сказать первым (нет смысла идти в baremetal для части или всех контуров).
func (d uiPageData) BaremetalSummary() *struct {
	Worthwhile bool
	Text       string
} {
	var no []string
	total := 0
	for _, r := range d.Results {
		if r.Packing == nil {
			continue
		}
		total++
		if !r.Packing.Verdict.Worthwhile {
			no = append(no, r.Env.Label)
		}
	}
	if total == 0 {
		return nil
	}
	min := DefaultPackingParams()
	minStr := fmt.Sprintf("%d × %d ядер / %d GiB", min.MinClusterNodes, min.MinServer.Cores, min.MinServer.RAMGiB)
	res := &struct {
		Worthwhile bool
		Text       string
	}{}
	switch {
	case len(no) == 0:
		res.Worthwhile = true
		res.Text = "Baremetal целесообразен: потребность каждого контура не меньше минимального кластера (" + minStr + ")."
	case len(no) == total:
		res.Text = "Baremetal нецелесообразен ни для одного контура: потребность меньше минимального кластера (" + minStr + "). Рекомендуем виртуальный managed K8s."
	default:
		res.Text = "Baremetal нецелесообразен для: " + strings.Join(no, ", ") + " — потребность меньше минимального кластера (" + minStr + "); для них — виртуальный managed K8s."
	}
	return res
}
