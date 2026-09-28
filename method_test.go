package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTable2Coefficients(t *testing.T) {
	tests := []struct {
		v             float64
		coef8, coef16 float64
		rng           string
	}{
		{0, 2.05, 1.50, "V ≤ 16"},
		{16, 2.05, 1.50, "V ≤ 16"}, // граница включительно
		{16.01, 1.61, 1.50, "16 < V ≤ 32"},
		{32, 1.61, 1.50, "16 < V ≤ 32"},
		{32.5, 1.45, 1.36, "32 < V ≤ 64"},
		{64, 1.45, 1.36, "32 < V ≤ 64"},
		{100, 1.35, 1.27, "64 < V ≤ 256"},
		{256, 1.35, 1.27, "64 < V ≤ 256"},
		{300, 1.34, 1.26, "256 < V ≤ 512"},
		{512, 1.34, 1.26, "256 < V ≤ 512"},
		{513, 1.33, 1.25, "V > 512"},
		{10000, 1.33, 1.25, "V > 512"},
	}
	for _, tt := range tests {
		c8, r8 := tableCoef(tt.v, 8)
		c16, r16 := tableCoef(tt.v, 16)
		if c8 != tt.coef8 || c16 != tt.coef16 || r8 != tt.rng || r16 != tt.rng {
			t.Errorf("V=%.2f: got (%.2f, %.2f, %q), want (%.2f, %.2f, %q)", tt.v, c8, c16, r8, tt.coef8, tt.coef16, tt.rng)
		}
	}
}

func TestTableMethodSteps(t *testing.T) {
	// Пример из прогона ПРОМ: V = 59.05 ядер, RAM 81.8 GiB.
	r := CalculateTableMethod(59_050, 83_810)
	if len(r.Variants) != 4 {
		t.Fatalf("вариантов %d, ожидали 4", len(r.Variants))
	}
	// 16 vCPU: 59.05 × 1.36 = 80.3 → ceil(80.3/16) = 6 нод; ×2 → 32 GiB × 6 = 192 ≥ 81.8.
	b := r.Best
	if b.NodeCPU != 16 || b.RAMFactor != 2 || b.Nodes != 6 || b.NominalRAMGiB != 192 || b.RAMBound {
		t.Errorf("best = %+v", b)
	}
	// 8 vCPU: 59.05 × 1.45 = 85.6 → 11 нод.
	for _, v := range r.Variants {
		if v.NodeCPU == 8 && v.Nodes != 11 {
			t.Errorf("8 vCPU: nodes = %d, ожидали 11", v.Nodes)
		}
	}
}

func TestBestVariantTieBreak(t *testing.T) {
	// Нод поровну, RAM поровну → выше плотность CPU (меньше ядер ёмкости).
	a := TableVariant{NodeCPU: 16, Nodes: 2, NominalRAMGiB: 64, CPUUtil: 30}
	b := TableVariant{NodeCPU: 8, Nodes: 2, NominalRAMGiB: 64, CPUUtil: 60}
	if got := bestTableVariant([]TableVariant{a, b}); got != b {
		t.Errorf("best = %+v, ожидали более плотный", got)
	}
	// Нод поровну → меньше RAM, даже при меньшей плотности CPU.
	c := TableVariant{NodeCPU: 16, Nodes: 2, NominalRAMGiB: 32, CPUUtil: 10}
	if got := bestTableVariant([]TableVariant{a, b, c}); got != c {
		t.Errorf("best = %+v, ожидали меньший RAM", got)
	}
}

func TestFmtChange(t *testing.T) {
	cases := map[string]string{
		fmtChange(59.05, 48.45, 2): "59.05 → 48.45 (−10.60, −18.0%)",
		fmtChange(6, 5, 0):         "6 → 5 (−1, −16.7%)",
		fmtChange(45.2, 47.3, 1):   "45.2 → 47.3 (+2.1, +4.6%)",
		fmtChange(10, 10, 0):       "10 → 10 (без изменений)",
		fmtChange(0, 3, 0):         "0 → 3 (+3)",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func dumpResults(t *testing.T) []EnvSizingResult {
	t.Helper()
	ds, err := parseDeploymentListJSON([]byte(`{"items":[
	 {"kind":"DeploymentConfig","metadata":{"name":"billing","namespace":"shop"},
	  "spec":{"replicas":4,"selector":{"app":"billing"},"template":{"metadata":{"labels":{"app":"billing"}},"spec":{"containers":[
	    {"name":"billing","image":"billing:1","resources":{"requests":{"cpu":"250m","memory":"512Mi"},"limits":{"cpu":"2","memory":"2Gi"}}}]}}}},
	 {"kind":"Deployment","metadata":{"name":"bad","namespace":"shop"},
	  "spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"bad","image":"","resources":{"requests":{"cpu":"1"},"limits":{"cpu":"500m"}}}]}}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	prom := promWith("medium")
	return []EnvSizingResult{CalculateEnvSizing(ds, prom, prom)}
}

func TestRenderManifests(t *testing.T) {
	files, warnings, err := RenderManifests(dumpResults(t))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"prom/namespace/shop.yaml", "prom/deployment/shop-billing.yaml", "prom/deployment/shop-bad.yaml"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	all := strings.Join(warnings, "\n")
	for _, w := range []string{"нет spec.selector", "image не задан", "cpu limit 500m < request 1"} {
		if !strings.Contains(all, w) {
			t.Errorf("нет предупреждения %q в:\n%s", w, all)
		}
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(files[1].Data, &obj); err != nil {
		t.Fatalf("манифест не JSON: %v", err)
	}
	if obj["kind"] != "Deployment" {
		t.Errorf("DC не сконвертирован: kind = %v", obj["kind"])
	}
}

func TestGenerateManifestsAndZip(t *testing.T) {
	results := dumpResults(t)
	dir := t.TempDir()
	m, err := GenerateManifests(dir, results)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 3 {
		t.Fatalf("files = %v", m.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "prom", "deployment", "shop-billing.yaml")); err != nil {
		t.Error(err)
	}

	var buf bytes.Buffer
	if _, err := WriteManifestsZip(&buf, results); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["prom/deployment/shop-billing.yaml"] || !names["WARNINGS.txt"] {
		t.Errorf("zip = %v", names)
	}
}

func TestPlanReportJSON(t *testing.T) {
	prom := promWith("off")
	env := prom
	rec, _ := ApplyOverrideLevel(MockDeployments(), prom, &env, "auto")
	r := CalculateEnvSizing(MockDeployments(), prom, env)
	r.Recommendation = rec
	rep := BuildPlanReport("mock", "all", prom, MockDeployments(), []EnvSizingResult{r})

	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "methodology", "basis", "envs", "data_warnings", "override_total"} {
		if _, ok := got[k]; !ok {
			t.Errorf("нет поля %s", k)
		}
	}
	e := rep.Envs[0]
	if e.OverrideLevel != "hard" || e.Recommendation == nil || e.OverrideRules == nil || e.Effect == nil {
		t.Errorf("env = level %s rec %v rules %v effect %v", e.OverrideLevel, e.Recommendation != nil, e.OverrideRules != nil, e.Effect != nil)
	}
	if e.Effect.Nodes.Before != 6 || e.Effect.Nodes.After != 5 || e.Effect.Nodes.Pct != -16.7 {
		t.Errorf("nodes = %+v", e.Effect.Nodes)
	}
	if len(e.Deployments) != 10 || e.Method.Best.Nodes != 5 {
		t.Errorf("plans = %d, best nodes = %d", len(e.Deployments), e.Method.Best.Nodes)
	}
}
