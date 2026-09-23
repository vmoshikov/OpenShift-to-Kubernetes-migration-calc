package main

// DefaultProfiles — стартовый набор коэффициентов для эмуляции контуров.
// Идея: ПРОМ-конфигурация (из OpenShift) — базовая (ReplicaFactor=1.0),
// остальные контуры урезаются по репликам и получают более щедрый CPU overcommit,
// т.к. там не нужна такая же отказоустойчивость и запас на пиковую нагрузку.
//
// Эти цифры — отправная точка для демо, в реальном проекте их нужно сверять
// с фактической практикой клиента (сколько реплик реально гоняют на DEV/IFT/ПСИ).
func DefaultProfiles() []EnvProfile {
	return []EnvProfile{
		{
			Key: "dev", Label: "DEV",
			ReplicaFactor: 0.2, MinReplicas: 1,
			CPUOvercommit: 3.0, HeadroomPercent: 0.10,
			SystemReserveCPU: 100, SystemReserveMem: 256,
		},
		{
			Key: "ift", Label: "IFT (тест)",
			ReplicaFactor: 0.4, MinReplicas: 1,
			CPUOvercommit: 2.5, HeadroomPercent: 0.10,
			SystemReserveCPU: 100, SystemReserveMem: 256,
		},
		{
			Key: "psi", Label: "ПСИ (приёмка)",
			ReplicaFactor: 0.7, MinReplicas: 2,
			CPUOvercommit: 1.5, HeadroomPercent: 0.15,
			SystemReserveCPU: 150, SystemReserveMem: 384,
		},
		{
			Key: "prom", Label: "ПРОМ",
			ReplicaFactor: 1.0, MinReplicas: 2,
			CPUOvercommit: 1.0, HeadroomPercent: 0.25,
			SystemReserveCPU: 200, SystemReserveMem: 512,
		},
	}
}
