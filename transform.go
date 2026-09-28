package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Workstream 1 общей архитектуры агента миграции: конвертация
// OpenShift-специфичных сущностей в K8s-эквиваленты — Route -> Ingress,
// DeploymentConfig -> Deployment, SCC -> Pod Security Admission labels.
// В отличие от sizing (Workstream 2, calculate.go/sizing.go), здесь
// источник только `oc` — эти API есть исключительно на OpenShift.

// TransformWarning — потеря информации при конвертации, требующая
// ручного ревью инженера после генерации манифестов.
type TransformWarning struct {
	Resource string
	Name     string
	Message  string
}

// RunTransform запускает конвертацию одного или всех типов сущностей
// и пишет получившиеся манифесты в outDir/<kind>/<ns>-<name>.yaml.
func RunTransform(kind, namespace, outDir string) error {
	if kind != "routes" && kind != "dc" && kind != "scc" && kind != "all" {
		return fmt.Errorf("неизвестный -transform=%q (ожидается routes | dc | scc | all)", kind)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("не удалось создать каталог %s: %w", outDir, err)
	}

	var warnings []TransformWarning
	var err error

	if kind == "all" || kind == "routes" {
		if warnings, err = transformRoutes(namespace, outDir, warnings); err != nil {
			return err
		}
	}
	if kind == "all" || kind == "dc" {
		if warnings, err = transformDeploymentConfigs(namespace, outDir, warnings); err != nil {
			return err
		}
	}
	if kind == "all" || kind == "scc" {
		if warnings, err = transformSCC(outDir, warnings); err != nil {
			return err
		}
	}

	fmt.Printf("Манифесты записаны в %s\n", outDir)
	printTransformWarnings(warnings)
	return nil
}

// --- oc exec + generic JSON helpers ------------------------------------

type genericList struct {
	Items []map[string]interface{} `json:"items"`
}

// ocGetJSON вызывает `oc get <resource> -o json` и возвращает items как
// generic map — полной типизации под все версии API OpenShift не нужно,
// нам важны лишь несколько полей каждой сущности.
func ocGetJSON(resource, namespace string) ([]map[string]interface{}, error) {
	args := []string{"get", resource, "-o", "json"}
	if resource == "scc" {
		// SCC — cluster-scoped, -A/-n не применимы.
	} else if namespace == "" || namespace == "all" {
		args = append(args, "-A")
	} else {
		args = append(args, "-n", namespace)
	}

	cmd := exec.Command("oc", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("не удалось выполнить oc get %s: %w\n%s", resource, err, string(out))
	}

	var list genericList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("не удалось распарсить вывод oc get %s: %w", resource, err)
	}
	return list.Items, nil
}

func mapGet(m map[string]interface{}, path ...string) interface{} {
	var cur interface{} = m
	for _, p := range path {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func strAt(m map[string]interface{}, path ...string) string {
	s, _ := mapGet(m, path...).(string)
	return s
}

// writeManifest пишет объект как pretty-printed JSON — это валидный
// подмножество YAML, `kubectl apply -f` принимает такие файлы напрямую.
func writeManifest(outDir, kind, namespace, name string, obj interface{}) error {
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(outDir, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fname := name
	if namespace != "" && namespace != name {
		fname = namespace + "-" + name
	}
	return os.WriteFile(filepath.Join(dir, fname+".yaml"), data, 0o644)
}

func printTransformWarnings(warnings []TransformWarning) {
	if len(warnings) == 0 {
		fmt.Println("Предупреждений нет.")
		return
	}
	fmt.Printf("\n=== Требуют ручного ревью (%d) ===\n", len(warnings))
	for _, w := range warnings {
		fmt.Printf("  [%s/%s] %s\n", w.Resource, w.Name, w.Message)
	}
}

// --- Route -> Ingress ----------------------------------------------------

func transformRoutes(namespace, outDir string, warnings []TransformWarning) ([]TransformWarning, error) {
	items, err := ocGetJSON("routes", namespace)
	if err != nil {
		return warnings, err
	}

	for _, item := range items {
		name := strAt(item, "metadata", "name")
		ns := strAt(item, "metadata", "namespace")
		host := strAt(item, "spec", "host")
		path := strAt(item, "spec", "path")
		if path == "" {
			path = "/"
		}
		toName := strAt(item, "spec", "to", "name")

		backendPort := map[string]interface{}{}
		switch v := mapGet(item, "spec", "port", "targetPort").(type) {
		case string:
			backendPort["name"] = v
		case float64:
			backendPort["number"] = int64(v)
		default:
			backendPort["number"] = int64(80)
			warnings = append(warnings, TransformWarning{"Route", name,
				"не удалось определить spec.port.targetPort, подставлен 80 — сверить с реальным Service"})
		}

		annotations := map[string]string{}
		var tlsBlock []map[string]interface{}
		if tlsMap, ok := mapGet(item, "spec", "tls").(map[string]interface{}); ok {
			termination, _ := tlsMap["termination"].(string)
			switch termination {
			case "edge", "reencrypt":
				tlsBlock = []map[string]interface{}{{
					"hosts":      []string{host},
					"secretName": name + "-tls",
				}}
				warnings = append(warnings, TransformWarning{"Route", name,
					fmt.Sprintf("termination=%s — создать Secret %s-tls с реальным сертификатом вручную", termination, name)})
			case "passthrough":
				annotations["nginx.ingress.kubernetes.io/ssl-passthrough"] = "true"
				warnings = append(warnings, TransformWarning{"Route", name,
					"termination=passthrough — проверить поддержку ssl-passthrough у ingress-контроллера целевого Mk8s"})
			}
		}

		ingressSpec := map[string]interface{}{
			"ingressClassName": "nginx", // TODO: подставить реальный класс ingress-контроллера целевого Mk8s
			"rules": []map[string]interface{}{{
				"host": host,
				"http": map[string]interface{}{
					"paths": []map[string]interface{}{{
						"path":     path,
						"pathType": "Prefix",
						"backend": map[string]interface{}{
							"service": map[string]interface{}{
								"name": toName,
								"port": backendPort,
							},
						},
					}},
				},
			}},
		}
		if len(tlsBlock) > 0 {
			ingressSpec["tls"] = tlsBlock
		}

		ingress := map[string]interface{}{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata": map[string]interface{}{
				"name":        name,
				"namespace":   ns,
				"annotations": annotations,
			},
			"spec": ingressSpec,
		}

		if err := writeManifest(outDir, "ingress", ns, name, ingress); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// --- DeploymentConfig -> Deployment ---------------------------------------

func transformDeploymentConfigs(namespace, outDir string, warnings []TransformWarning) ([]TransformWarning, error) {
	items, err := ocGetJSON("deploymentconfigs", namespace)
	if err != nil {
		return warnings, err
	}

	for _, item := range items {
		deployment, ws := dcToDeployment(item)
		warnings = append(warnings, ws...)
		if err := writeManifest(outDir, "deployment", strAt(item, "metadata", "namespace"), strAt(item, "metadata", "name"), deployment); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// dcToDeployment конвертирует один DeploymentConfig в apps/v1 Deployment.
// Используется и -transform=dc, и генерацией манифестов по плану sizing.
func dcToDeployment(item map[string]interface{}) (map[string]interface{}, []TransformWarning) {
	var warnings []TransformWarning
	name := strAt(item, "metadata", "name")
	ns := strAt(item, "metadata", "namespace")
	labels := mapGet(item, "metadata", "labels")

	replicas := int64(1)
	if r, ok := mapGet(item, "spec", "replicas").(float64); ok {
		replicas = int64(r)
	}

	selectorMap, _ := mapGet(item, "spec", "selector").(map[string]interface{})
	if len(selectorMap) == 0 {
		if tplLabels, ok := mapGet(item, "spec", "template", "metadata", "labels").(map[string]interface{}); ok {
			selectorMap = tplLabels
		}
		warnings = append(warnings, TransformWarning{"DeploymentConfig", name,
			"spec.selector пуст — matchLabels взят из template.metadata.labels, сверить вручную"})
	}

	strategyType := strAt(item, "spec", "strategy", "type")
	deployStrategy := "RollingUpdate"
	if strategyType == "Recreate" {
		deployStrategy = "Recreate"
	}

	if triggers, ok := mapGet(item, "spec", "triggers").([]interface{}); ok && len(triggers) > 0 {
		warnings = append(warnings, TransformWarning{"DeploymentConfig", name,
			fmt.Sprintf("%d триггеров (ImageChange/ConfigChange) не переносятся в Deployment — обновление образа перенести в CI/CD пайплайн", len(triggers))})
	}

	deployment := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"replicas": replicas,
			"selector": map[string]interface{}{
				"matchLabels": selectorMap,
			},
			"strategy": map[string]interface{}{
				"type": deployStrategy,
			},
			"template": mapGet(item, "spec", "template"),
		},
	}
	return deployment, warnings
}

// --- SCC -> Pod Security Admission ----------------------------------------

// sccToPSALevel — приблизительное сопоставление стандартных SCC с уровнями PSA.
// Точное соответствие невозможно (PSA грубее модели SCC), это отправная точка
// для ручной сверки, не готовый security-контракт.
var sccToPSALevel = map[string]string{
	"privileged":       "privileged",
	"hostnetwork":      "privileged",
	"hostaccess":       "privileged",
	"hostmount-anyuid": "privileged",
	"anyuid":           "baseline",
	"nonroot":          "restricted",
	"nonroot-v2":       "restricted",
	"restricted":       "restricted",
	"restricted-v2":    "restricted",
}

func psaLevelRank(l string) int {
	switch l {
	case "privileged":
		return 3
	case "baseline":
		return 2
	case "restricted":
		return 1
	default:
		return 0
	}
}

func transformSCC(outDir string, warnings []TransformWarning) ([]TransformWarning, error) {
	items, err := ocGetJSON("scc", "")
	if err != nil {
		return warnings, err
	}

	// Для каждого namespace берём самый широкий (least restrictive) уровень
	// среди всех SCC, назначенных его serviceaccount'ам — PSA действует
	// на весь namespace целиком, в отличие от SCC, который выдаётся per-SA.
	nsLevel := map[string]string{}
	applyLevel := func(ns, level string) {
		if cur, ok := nsLevel[ns]; !ok || psaLevelRank(level) > psaLevelRank(cur) {
			nsLevel[ns] = level
		}
	}

	for _, item := range items {
		name := strAt(item, "metadata", "name")
		level, known := sccToPSALevel[name]
		if !known {
			level = "baseline"
			warnings = append(warnings, TransformWarning{"SCC", name,
				"неизвестная кастомная SCC — по умолчанию сопоставлена с baseline, сверить права вручную (privileged/hostPath/capabilities)"})
		}

		for _, key := range []string{"users", "groups"} {
			arr, ok := mapGet(item, key).([]interface{})
			if !ok {
				continue
			}
			for _, v := range arr {
				s, _ := v.(string)
				switch {
				case strings.HasPrefix(s, "system:serviceaccount:"):
					parts := strings.SplitN(strings.TrimPrefix(s, "system:serviceaccount:"), ":", 2)
					if len(parts) == 2 {
						applyLevel(parts[0], level)
					}
				case strings.HasPrefix(s, "system:serviceaccounts:"):
					if ns := strings.TrimPrefix(s, "system:serviceaccounts:"); ns != "" {
						applyLevel(ns, level)
					}
				}
			}
		}
	}

	if len(nsLevel) == 0 {
		warnings = append(warnings, TransformWarning{"SCC", "-",
			"не найдено ни одной привязки SCC к serviceaccount — PSA labels не сгенерированы, разметить namespace вручную"})
	}

	namespaces := make([]string, 0, len(nsLevel))
	for ns := range nsLevel {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	for _, ns := range namespaces {
		level := nsLevel[ns]
		manifest := map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": ns,
				"labels": map[string]string{
					"pod-security.kubernetes.io/enforce": level,
					"pod-security.kubernetes.io/audit":   level,
					"pod-security.kubernetes.io/warn":    level,
				},
			},
		}
		if err := writeManifest(outDir, "namespace-psa", ns, ns, manifest); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}
