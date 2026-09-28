package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Ручной ввод стенда без обращения к живому кластеру — для закрытых
// контуров или когда доступа к oc/kubectl нет, а есть только выгрузка
// (JSON-дамп или произвольный текст), которую можно вставить в терминал.

// ReadDeploymentsFromStdinJSON читает из stdin JSON в том же формате, что
// `kubectl/oc get deployments -A -o json` — то есть можно снять дамп на
// стороне клиента и вставить его сюда, не открывая доступ к кластеру.
func ReadDeploymentsFromStdinJSON() ([]DeploymentSpec, error) {
	fmt.Fprintln(os.Stderr, "Вставьте JSON (oc/kubectl get deployments -A -o json) и завершите ввод Ctrl+D:")
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать stdin: %w", err)
	}
	return DeploymentsFromPastedJSON(string(data))
}

// DeploymentsFromPastedJSON — тот же разбор, что ReadDeploymentsFromStdinJSON,
// но для уже полученной откуда-то (не обязательно stdin) строки — используется
// и CLI-режимом `-source=paste`, и веб-UI (`web.go`).
func DeploymentsFromPastedJSON(raw string) ([]DeploymentSpec, error) {
	deployments, err := parseDeploymentListJSON([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("не удалось распарсить вставленный JSON: %w", err)
	}
	if len(deployments) == 0 {
		return nil, fmt.Errorf("во вставленном JSON не найдено ни одного деплоймента")
	}
	return deployments, nil
}

// ReadDeploymentsFromLLM читает из stdin произвольный текстовый дамп ресурсов
// OpenShift (вывод `oc get all`, `oc describe dc`, таблицу requests/limits,
// кусок YAML-манифеста и т.п.) и просит LLM извлечь из него структурированный
// список деплойментов — для случаев, когда под рукой нет чистого JSON-экспорта.
func ReadDeploymentsFromLLM(cfg ReviewConfig) ([]DeploymentSpec, error) {
	fmt.Fprintln(os.Stderr, "Вставьте выгрузку по деплойментам OpenShift (oc get all, oc describe dc, "+
		"таблицу requests/limits и т.п.) и завершите ввод Ctrl+D:")
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать stdin: %w", err)
	}
	return DeploymentsFromLLMExtraction(cfg, string(raw))
}

// DeploymentsFromLLMExtraction — тот же разбор через LLM, что
// ReadDeploymentsFromLLM, но для уже полученного откуда-то (не обязательно
// stdin) текста — используется и CLI-режимом `-source=llm-paste`, и веб-UI.
func DeploymentsFromLLMExtraction(cfg ReviewConfig, raw string) ([]DeploymentSpec, error) {
	if len(bytes.TrimSpace([]byte(raw))) == 0 {
		return nil, fmt.Errorf("пустой ввод")
	}

	prompt := "Ты извлекаешь структурированные данные о деплойментах OpenShift из произвольного текста " +
		"(вывод oc get all, oc describe dc, таблицы requests/limits, YAML-манифесты и т.п.).\n" +
		"Верни ТОЛЬКО валидный JSON-массив без markdown-обёртки и пояснений, каждый элемент вида:\n" +
		`{"name": "...", "namespace": "...", "replicas": N, "containers": [{"name": "...", ` +
		`"cpu_request_milli": N, "mem_request_mib": N, "cpu_limit_milli": N, "mem_limit_mib": N}]}` + "\n" +
		"cpu_request_milli — запрос CPU в миллиядрах (1000 = 1 vCPU, \"250m\" -> 250, \"1\" -> 1000).\n" +
		"mem_request_mib — запрос памяти в MiB (\"512Mi\" -> 512, \"1Gi\" -> 1024).\n" +
		"Если значения нет в тексте — поставь 0. Не придумывай сущности, которых нет в тексте.\n\n" +
		"Текст:\n" + raw

	reply, err := callChatCompletion(cfg, prompt)
	if err != nil {
		return nil, fmt.Errorf("извлечение через LLM не удалось: %w", err)
	}

	deployments, err := parseLLMDeploymentJSON(reply)
	if err != nil {
		return nil, err
	}
	if len(deployments) == 0 {
		return nil, fmt.Errorf("модель не нашла ни одного деплоймента во введённом тексте")
	}
	return deployments, nil
}

type llmContainer struct {
	Name            string `json:"name"`
	CPURequestMilli int64  `json:"cpu_request_milli"`
	MemRequestMiB   int64  `json:"mem_request_mib"`
	CPULimitMilli   int64  `json:"cpu_limit_milli"`
	MemLimitMiB     int64  `json:"mem_limit_mib"`
}

type llmDeployment struct {
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace"`
	Replicas   int            `json:"replicas"`
	Containers []llmContainer `json:"containers"`
}

func parseLLMDeploymentJSON(reply string) ([]DeploymentSpec, error) {
	reply = stripCodeFence(reply)

	var parsed []llmDeployment
	if err := json.Unmarshal([]byte(reply), &parsed); err != nil {
		return nil, fmt.Errorf("не удалось распарсить JSON от модели: %w\nответ модели:\n%s", err, reply)
	}

	deployments := make([]DeploymentSpec, 0, len(parsed))
	for _, d := range parsed {
		replicas := d.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		namespace := d.Namespace
		if namespace == "" {
			namespace = "manual-input"
		}

		containers := make([]ContainerSpec, 0, len(d.Containers))
		for _, c := range d.Containers {
			containers = append(containers, ContainerSpec{
				Name:     c.Name,
				Requests: ResourceSpec{CPUMilli: c.CPURequestMilli, MemMiB: c.MemRequestMiB},
				Limits:   ResourceSpec{CPUMilli: c.CPULimitMilli, MemMiB: c.MemLimitMiB},
			})
		}

		deployments = append(deployments, DeploymentSpec{
			Name:       d.Name,
			Namespace:  namespace,
			Kind:       "Deployment",
			Replicas:   replicas,
			Containers: containers,
		})
	}
	return deployments, nil
}

// stripCodeFence снимает возможную обёртку ```json ... ``` из ответа модели,
// если она проигнорировала просьбу вернуть чистый JSON.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
