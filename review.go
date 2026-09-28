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
// просим найти риски (SPOF на DEV/IFT, агрессивный override, подозрительно
// большие лимиты у legacy-сервисов и т.п.) и дать короткую рекомендацию.
func ReviewSizingPlan(cfg ReviewConfig, results []EnvSizingResult) (string, error) {
	var sb strings.Builder
	sb.WriteString("Ты — SRE-инженер, проверяющий план миграции с OpenShift на managed Kubernetes.\n")
	sb.WriteString("Вот расчёт capacity по контурам (DEV/IFT/ПСИ/ПРОМ) по методике: V = сумма limits подов, " +
		"размер кластера = V × коэффициент Таблицы 2, ноды 8/16 vCPU, RAM ноды ×2/×4 от ядер, не менее 2 нод.\n")
	sb.WriteString("Проверь план критически:\n")
	sb.WriteString("1) есть ли риск нехватки ресурсов при пиковой нагрузке;\n")
	sb.WriteString("2) не слишком ли агрессивен override limits (риск CPU throttling и OOMKill);\n")
	sb.WriteString("3) достаточно ли нод для отказоустойчивости на ПСИ/ПРОМ;\n")
	sb.WriteString("4) любые другие риски миграции.\n")
	sb.WriteString("Отвечай кратко, по пунктам, на русском.\n")
	// TODO(prometheus): когда появятся метрики — передавать p95/p99 и throttling
	// по крупнейшим сервисам, чтобы модель проверяла override по нагрузке.
	sb.WriteString("Важно: базис — limits из манифестов, фактическое потребление (Prometheus) НЕ подключено; " +
		"укажи, где сжатие limits рискованно без метрик.\n\n")

	for _, r := range results {
		t := r.Table
		sb.WriteString(fmt.Sprintf(
			"[%s] pods=%d, V=%.2f cores, RAM=%.1fGiB, coef=%.2f (%s), nodes=%d x %s, nominal=%dcores/%dGiB, density_CPU=%.0f%%, density_RAM=%.0f%%\n",
			r.Env.Label, r.TotalPods, t.UserCores, t.UserRAMGiB, t.Best.Coef, t.Best.CoefRange,
			t.Best.Nodes, t.Best.Name(), t.Best.NominalCores(), t.Best.NominalRAMGiB, t.Best.CPUUtil, t.Best.RAMUtil,
		))
	}

	for _, r := range results {
		if !r.Env.Override.Enabled() {
			continue
		}
		e := r.Effect()
		sb.WriteString(fmt.Sprintf("[%s] override=%s: V %s cores; RAM %s GiB; nodes %s\n", e.Label, e.Level,
			fmtChange(e.CoresBefore, e.CoresAfter, 2), fmtChange(e.RAMBefore, e.RAMAfter, 1),
			fmtChange(float64(e.NodesBefore), float64(e.NodesAfter), 0)))
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
