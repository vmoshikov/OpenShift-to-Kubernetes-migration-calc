package main

import (
	"fmt"
	"html/template"
	"net/http"
)

// Минимальный веб-UI поверх тех же функций, что и CLI (CalculateEnvSizing,
// DeploymentsFromPastedJSON, DeploymentsFromLLMExtraction) — форма для
// ручного ввода ресурсов стенда прямо в окне браузера, без консоли.
// Один статический HTML, без JS/CSS-фреймворков — чтобы не тянуть
// зависимости в демо, которое должно собираться и работать офлайн.

var uiTemplate = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<title>mk8s-calc</title>
<style>
 body{font-family:system-ui,sans-serif;max-width:920px;margin:2rem auto;padding:0 1rem;color:#1a1a1a}
 h1{font-size:1.3rem}
 h2{font-size:1.05rem;margin-top:2rem}
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

  <label>Контур</label>
  <select name="env">
   <option value="all" {{if eq .Env "all"}}selected{{end}}>все контуры</option>
   <option value="dev" {{if eq .Env "dev"}}selected{{end}}>DEV</option>
   <option value="ift" {{if eq .Env "ift"}}selected{{end}}>IFT (тест)</option>
   <option value="psi" {{if eq .Env "psi"}}selected{{end}}>ПСИ (приёмка)</option>
   <option value="prom" {{if eq .Env "prom"}}selected{{end}}>ПРОМ</option>
  </select>

  <label class="checkbox"><input type="checkbox" name="review" value="1" {{if .Review}}checked{{end}}> ревью плана LLM (нужен OPENAI_API_KEY на сервере)</label>

  <p><button type="submit">Посчитать</button></p>
 </fieldset>
</form>

{{if .Error}}<div class="err">{{.Error}}</div>{{end}}

{{if .Results}}
<h2>Результат</h2>
<p>Деплойментов: {{.DeploymentCount}} · Подов (baseline): {{.TotalPods}}</p>
<table>
<tr><th>Контур</th><th>Подов</th><th>CPU спрос</th><th>Mem спрос</th><th>Пул узлов</th><th>CPU util</th><th>Mem util</th><th>Оптимизация</th></tr>
{{range .Results}}
<tr>
 <td>{{.Env.Label}}</td>
 <td>{{.TotalPods}}</td>
 <td>{{.DemandCPUMilli}}m</td>
 <td>{{.DemandMemMiB}}MiB</td>
 <td>{{.PoolSummary}}</td>
 <td>{{printf "%.1f" .CPUUtilization}}%</td>
 <td>{{printf "%.1f" .MemUtilization}}%</td>
 <td>{{printf "%.1f" .OptimizationPercent}}%</td>
</tr>
{{end}}
</table>
<p class="total">Итого оптимизация (смешанные пулы vs однородные): {{printf "%.1f" .TotalOptimization}}%</p>
{{end}}

{{if .ReviewText}}
<h2>Ревью модели</h2>
<pre>{{.ReviewText}}</pre>
{{end}}

</body>
</html>
`))

type uiPageData struct {
	Source            string
	Env               string
	PasteInput        string
	Review            bool
	Error             string
	Results           []EnvSizingResult
	DeploymentCount   int
	TotalPods         int
	TotalOptimization float64
	ReviewText        string
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
	data := uiPageData{Source: "mock", Env: "all"}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			data.Error = "не удалось разобрать форму: " + err.Error()
			renderUI(w, data)
			return
		}
		data.Source = r.FormValue("source")
		data.Env = r.FormValue("env")
		data.PasteInput = r.FormValue("paste")
		data.Review = r.FormValue("review") == "1"

		deployments, err := loadDeploymentsForUI(data.Source, data.PasteInput)
		if err != nil {
			data.Error = err.Error()
			renderUI(w, data)
			return
		}

		data.DeploymentCount = len(deployments)
		for _, d := range deployments {
			data.TotalPods += d.Replicas
		}

		flavors := DefaultFlavors()
		var costSum, baselineSum float64
		for _, env := range DefaultProfiles() {
			if data.Env != "all" && data.Env != env.Key {
				continue
			}
			res := CalculateEnvSizing(deployments, env, flavors)
			data.Results = append(data.Results, res)
			costSum += res.costPerHour
			baselineSum += res.baselineCostPerHour
		}
		if baselineSum > 0 {
			data.TotalOptimization = 100 * (baselineSum - costSum) / baselineSum
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
