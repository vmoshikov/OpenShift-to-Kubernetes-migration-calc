package main

import "fmt"

// DefaultProfiles — стартовый набор коэффициентов для эмуляции контуров.
// Идея: ПРОМ-конфигурация (из OpenShift) — базовая (ReplicaFactor=1.0),
// остальные контуры урезаются по репликам, т.к. там не нужна такая же
// отказоустойчивость. Запас под сервисные расходы даёт коэффициент Таблицы 2.
//
// Эти цифры — отправная точка для демо, в реальном проекте их нужно сверять
// с фактической практикой клиента (сколько реплик реально гоняют на DEV/IFT/ПСИ).
func DefaultProfiles() []EnvProfile {
	profiles := []EnvProfile{
		{
			Key: "dev", Label: "DEV",
			ReplicaFactor: 0.2, MinReplicas: 1,
		},
		{
			Key: "ift", Label: "IFT (тест)",
			ReplicaFactor: 0.4, MinReplicas: 1,
		},
		{
			Key: "psi", Label: "ПСИ (приёмка)",
			ReplicaFactor: 0.7, MinReplicas: 2,
		},
		{
			Key: "prom", Label: "ПРОМ",
			ReplicaFactor: 1.0, MinReplicas: 2,
		},
	}
	for i := range profiles {
		profiles[i].Override = DefaultLimitsOverride()
	}
	return profiles
}

// ProfileByKey ищет профиль контура по ключу (dev | ift | psi | prom).
func ProfileByKey(profiles []EnvProfile, key string) (EnvProfile, error) {
	for _, p := range profiles {
		if p.Key == key {
			return p, nil
		}
	}
	return EnvProfile{}, fmt.Errorf("неизвестный стенд %q (ожидается dev | ift | psi | prom)", key)
}
