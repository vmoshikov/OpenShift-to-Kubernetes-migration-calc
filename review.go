package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ReviewConfig собирается из переменных окружения, чтобы можно было подставить
// любой OpenAI-совместимый эндпоинт: сам OpenAI, Azure OpenAI прокси,
// self-hosted vLLM/LM Studio и т.д. — без изменения кода.
type ReviewConfig struct {
	APIKey  string // OPENAI_API_KEY
	BaseURL string // OPENAI_BASE_URL, напр. https://api.openai.com/v1
	Model   string // OPENAI_MODEL, напр. gpt-4.1-mini
}

func LoadReviewConfigFromEnv() ReviewConfig {
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return ReviewConfig{
		APIKey:  os.Getenv("OPENAI_API_KEY"),
		BaseURL: strings.TrimRight(base, "/"),
		Model:   model,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ReviewSizingPlan отправляет сводку по всем контурам на ревью модели:
// просим найти риски (недостаточный headroom, SPOF на DEV/IFT, неоптимальный
// подбор флейвора, подозрительно большие лимиты у legacy-сервисов и т.п.)
// и дать короткую рекомендацию человеческим языком.
func ReviewSizingPlan(cfg ReviewConfig, results []EnvSizingResult) (string, error) {
	var sb strings.Builder
	sb.WriteString("Ты — SRE-инженер, проверяющий план миграции с OpenShift на managed Kubernetes.\n")
	sb.WriteString("Вот расчёт capacity по контурам (DEV/IFT/ПСИ/ПРОМ). Проверь план критически:\n")
	sb.WriteString("1) есть ли риск нехватки ресурсов при пиковой нагрузке;\n")
	sb.WriteString("2) не занижен ли headroom на ПРОМ;\n")
	sb.WriteString("3) есть ли смысл сменить флейвор узла для экономии;\n")
	sb.WriteString("4) любые другие риски миграции.\n")
	sb.WriteString("Отвечай кратко, по пунктам, на русском.\n\n")

	for _, r := range results {
		sb.WriteString(fmt.Sprintf(
			"[%s] pods=%d, CPU=%dm, Mem=%dMiB, pool=%s, CPU_util=%.1f%%, Mem_util=%.1f%%, optimization=%.1f%%\n",
			r.Env.Label, r.TotalPods, r.DemandCPUMilli, r.DemandMemMiB,
			r.PoolSummary(), r.CPUUtilization, r.MemUtilization, r.OptimizationPercent,
		))
	}

	return callChatCompletion(cfg, sb.String())
}

// callChatCompletion — общий вызов OpenAI-совместимого /chat/completions,
// используется и для ревью плана (ReviewSizingPlan), и для извлечения
// деплойментов из произвольного текстового дампа (ReadDeploymentsFromLLM).
func callChatCompletion(cfg ReviewConfig, prompt string) (string, error) {
	if cfg.APIKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY не задан")
	}

	reqBody := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.2,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest(http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("запрос к %s не выполнен: %w", cfg.BaseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("не удалось распарсить ответ модели: %w\nсырой ответ: %s", err, string(body))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("ошибка API: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("пустой ответ модели, сырой ответ: %s", string(body))
	}
	return parsed.Choices[0].Message.Content, nil
}
