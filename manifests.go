package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Генерация манифестов под целевой контур по плану sizing: реплики
// пересчитаны от стенда-источника, limits — после override. Исходный объект
// из дампа (image, env, probes, volumes) сохраняется, меняются только
// replicas и сжатые limits; служебные поля кластера-источника вычищаются.
// Файлы: <out>/<env>/<kind>/<namespace>-<name>.yaml (JSON — подмножество YAML),
// плюс <out>/<env>/namespace/<ns>.yaml на каждый namespace.

// ManifestResult — что сгенерировано и что требует ручного ревью.
type ManifestResult struct {
	Dir      string   `json:"dir"`
	Files    []string `json:"files"`
	Warnings []string `json:"warnings"`
}

const annPrefix = "mk8s-calc/"

// ManifestFile — один сгенерированный манифест: относительный путь и содержимое.
type ManifestFile struct {
	Path string // <env>/<kind>/<ns>-<name>.yaml
	Data []byte
}

// RenderManifests собирает манифесты всех посчитанных контуров в память:
// Namespace на каждый namespace контура + workload'ы. Общая часть для
// записи в каталог (-manifests) и zip-выгрузки из веб-UI.
func RenderManifests(results []EnvSizingResult) ([]ManifestFile, []string, error) {
	var files []ManifestFile
	warnings := []string{}
	add := func(env, kind, ns, name string, obj interface{}) error {
		data, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return err
		}
		fname := name
		if ns != "" && ns != name {
			fname = ns + "-" + name
		}
		files = append(files, ManifestFile{Path: filepath.Join(env, kind, fname+".yaml"), Data: append(data, '\n')})
		return nil
	}

	for _, r := range results {
		// Namespace'ы — первыми: без них apply workload'ов упадёт на пустом кластере.
		seen := map[string]bool{}
		for _, p := range r.Deployments {
			if p.Namespace == "" || seen[p.Namespace] {
				continue
			}
			seen[p.Namespace] = true
			// TODO: переносить LimitRange/ResourceQuota namespace'а-источника —
			// на OpenShift они подставляли limits по умолчанию, без них поды
			// без явных limits приедут неограниченными.
			if err := add(r.Env.Key, "namespace", "", p.Namespace, namespaceManifest(p.Namespace, r.Env.Key)); err != nil {
				return nil, nil, err
			}
		}
		for _, p := range r.Deployments {
			obj, ws := buildManifest(p, r.Env)
			ws = append(ws, validateManifest(obj)...)
			for _, w := range ws {
				warnings = append(warnings, fmt.Sprintf("[%s] %s/%s: %s", r.Env.Key, p.Namespace, p.Name, w))
			}
			kind, _ := obj["kind"].(string)
			if err := add(r.Env.Key, strings.ToLower(kind), p.Namespace, p.Name, obj); err != nil {
				return nil, nil, err
			}
		}
	}
	return files, warnings, nil
}

// GenerateManifests пишет манифесты всех посчитанных контуров в outDir.
func GenerateManifests(outDir string, results []EnvSizingResult) (ManifestResult, error) {
	files, warnings, err := RenderManifests(results)
	res := ManifestResult{Dir: outDir, Files: []string{}, Warnings: warnings}
	if err != nil {
		return res, err
	}
	for _, f := range files {
		path := filepath.Join(outDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			return res, err
		}
		res.Files = append(res.Files, path)
	}
	return res, nil
}

// WriteManifestsZip пишет манифесты zip-архивом (выгрузка из веб-UI).
func WriteManifestsZip(w io.Writer, results []EnvSizingResult) ([]string, error) {
	files, warnings, err := RenderManifests(results)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(w)
	now := time.Now()
	for _, f := range files {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: filepath.ToSlash(f.Path), Method: zip.Deflate, Modified: now})
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if len(warnings) > 0 {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: "WARNINGS.txt", Method: zip.Deflate, Modified: now})
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(fw, strings.Join(warnings, "\n")+"\n"); err != nil {
			return nil, err
		}
	}
	return warnings, zw.Close()
}

func namespaceManifest(ns, env string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":        ns,
			"annotations": map[string]interface{}{annPrefix + "env": env},
		},
	}
}

// validateManifest — самопроверка того, что K8s отвергнет или что не
// заработает: нет selector/контейнеров/image, limit ниже request.
// Не замена `kubectl apply --dry-run=server` на целевом кластере.
func validateManifest(obj map[string]interface{}) []string {
	var ws []string
	if mapGet(obj, "spec", "selector") == nil {
		ws = append(ws, "нет spec.selector — apps/v1 его требует")
	}
	containers, _ := mapGet(obj, "spec", "template", "spec", "containers").([]interface{})
	if len(containers) == 0 {
		ws = append(ws, "нет контейнеров в spec.template.spec.containers")
	}
	for _, ci := range containers {
		c, _ := ci.(map[string]interface{})
		name := strAt(c, "name")
		if img := strings.TrimSpace(strAt(c, "image")); img == "" || img == "REPLACE_ME" {
			ws = append(ws, fmt.Sprintf("контейнер %s: image не задан", name))
		}
		for _, res := range []string{"cpu", "memory"} {
			req, _ := mapGet(c, "resources", "requests", res).(string)
			lim, _ := mapGet(c, "resources", "limits", res).(string)
			if req == "" || lim == "" {
				continue
			}
			var rq, lm int64
			if res == "cpu" {
				rq, lm = k8sQuantity(req).toMilliCPU(), k8sQuantity(lim).toMilliCPU()
			} else {
				rq, lm = k8sQuantity(req).toMiB(), k8sQuantity(lim).toMiB()
			}
			if lm < rq {
				ws = append(ws, fmt.Sprintf("контейнер %s: %s limit %s < request %s — K8s отвергнет", name, res, lim, req))
			}
		}
	}
	return ws
}

// buildManifest собирает объект одного workload для контура env.
func buildManifest(p DeploymentPlan, env EnvProfile) (map[string]interface{}, []string) {
	var warnings []string
	var obj map[string]interface{}

	switch {
	case p.src == nil || p.src.Raw == nil:
		obj = skeletonManifest(p)
		warnings = append(warnings, "исходного манифеста нет (mock/llm-paste) — сгенерирован скелет, заполнить image, probes, env")
	case p.Kind == "DeploymentConfig":
		var tws []TransformWarning
		obj, tws = dcToDeployment(deepCopy(p.src.Raw))
		for _, w := range tws {
			warnings = append(warnings, w.Message)
		}
	default:
		obj = deepCopy(p.src.Raw)
	}

	cleanObject(obj)
	spec, _ := obj["spec"].(map[string]interface{})
	if spec == nil {
		spec = map[string]interface{}{}
		obj["spec"] = spec
	}
	spec["replicas"] = p.Replicas

	ann := ensureMap(ensureMap(obj, "metadata"), "annotations")
	ann[annPrefix+"env"] = env.Key
	ann[annPrefix+"source-replicas"] = fmt.Sprint(p.FromReplicas)

	var changed []string
	containers, _ := mapGet(obj, "spec", "template", "spec", "containers").([]interface{})
	for _, ci := range containers {
		c, ok := ci.(map[string]interface{})
		if !ok {
			continue
		}
		cp, ok := findContainer(p, strAt(c, "name"))
		if !ok {
			continue
		}
		if cp.CPUOverridden || cp.MemOverridden {
			limits := ensureMap(ensureMap(c, "resources"), "limits")
			if cp.CPUOverridden {
				changed = append(changed, fmt.Sprintf("%s cpu %s", cp.Name, fmtCPU(cp.OrigLimits.CPUMilli)))
				limits["cpu"] = fmtCPU(cp.Limits.CPUMilli)
			}
			if cp.MemOverridden {
				changed = append(changed, fmt.Sprintf("%s memory %s", cp.Name, fmtMem(cp.OrigLimits.MemMiB)))
				limits["memory"] = fmtMem(cp.Limits.MemMiB)
			}
		}
		if img := strAt(c, "image"); isOpenShiftInternalImage(img) {
			warnings = append(warnings, fmt.Sprintf("контейнер %s: образ из внутреннего реестра OpenShift (%s) — перенести в реестр, доступный managed K8s", cp.Name, img))
		}
	}
	if len(changed) > 0 {
		ann[annPrefix+"limits-override"] = env.Override.Level
		ann[annPrefix+"original-limits"] = strings.Join(changed, "; ")
		// TODO(prometheus): сюда же — p99 потребления, на который опирался override,
		// чтобы ревьюер манифеста видел обоснование, а не только формулу.
	}

	if vcts, ok := mapGet(obj, "spec", "volumeClaimTemplates").([]interface{}); ok && len(vcts) > 0 {
		warnings = append(warnings, "volumeClaimTemplates: storageClassName и данные томов не переносятся автоматически — сверить StorageClass целевого кластера и план миграции данных")
	}
	return obj, warnings
}

func findContainer(p DeploymentPlan, name string) (ContainerPlan, bool) {
	for _, c := range p.Containers {
		if c.Name == name {
			return c, true
		}
	}
	return ContainerPlan{}, false
}

// skeletonManifest — Deployment без исходного объекта (mock/llm-paste).
func skeletonManifest(p DeploymentPlan) map[string]interface{} {
	labels := map[string]interface{}{"app": p.Name}
	var containers []interface{}
	for _, c := range p.Containers {
		requests := map[string]interface{}{}
		limits := map[string]interface{}{}
		if c.Requests.CPUMilli > 0 {
			requests["cpu"] = fmtCPU(c.Requests.CPUMilli)
		}
		if c.Requests.MemMiB > 0 {
			requests["memory"] = fmtMem(c.Requests.MemMiB)
		}
		// В скелет идут только реально заданные limits (с override);
		// подстановка requests вместо отсутствующего limit — только для базиса.
		if !c.CPUNoLimit {
			limits["cpu"] = fmtCPU(c.Limits.CPUMilli)
		}
		if !c.MemNoLimit {
			limits["memory"] = fmtMem(c.Limits.MemMiB)
		}
		containers = append(containers, map[string]interface{}{
			"name":      c.Name,
			"image":     "REPLACE_ME",
			"resources": map[string]interface{}{"requests": requests, "limits": limits},
		})
	}
	return map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": p.Name, "namespace": p.Namespace, "labels": labels},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{"matchLabels": labels},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": labels},
				"spec":     map[string]interface{}{"containers": containers},
			},
		},
	}
}

// cleanObject убирает состояние и служебные поля кластера-источника,
// которые нельзя (или бессмысленно) применять в другой кластер.
func cleanObject(obj map[string]interface{}) {
	delete(obj, "status")
	md, _ := obj["metadata"].(map[string]interface{})
	if md == nil {
		return
	}
	for _, k := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink", "ownerReferences"} {
		delete(md, k)
	}
	if ann, ok := md["annotations"].(map[string]interface{}); ok {
		for k := range ann {
			if k == "kubectl.kubernetes.io/last-applied-configuration" ||
				k == "deployment.kubernetes.io/revision" ||
				strings.HasPrefix(k, "openshift.io/") {
				delete(ann, k)
			}
		}
	}
}

// isOpenShiftInternalImage — образ из встроенного реестра OpenShift,
// который не будет доступен из managed K8s.
func isOpenShiftInternalImage(img string) bool {
	return strings.Contains(img, "image-registry.openshift-image-registry.svc") ||
		strings.Contains(img, "docker-registry.default.svc")
}

func fmtCPU(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprint(m / 1000)
	}
	return fmt.Sprintf("%dm", m)
}

func fmtMem(mib int64) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGi", mib/1024)
	}
	return fmt.Sprintf("%dMi", mib)
}

func ensureMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	v := map[string]interface{}{}
	m[key] = v
	return v
}

func deepCopy(m map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(m)
	var out map[string]interface{}
	_ = json.Unmarshal(data, &out)
	return out
}
